// Package secrets fetches job secrets from external secret managers and
// mints registry credentials, on the runner, just before a job runs.
//
// Identity comes from the job's OIDC token (FORGE_OIDC_TOKEN) wherever the
// manager can trust it — Vault's JWT auth, AWS STS web identity, GCP
// workload identity federation, Azure workload identity federation — so no
// static cloud keys are stored in Forge. When a job carries no token, or a
// manager isn't set up for OIDC, the runner's own ambient identity is used
// (VAULT_TOKEN, the AWS credential chain, GOOGLE_OAUTH_ACCESS_TOKEN,
// AZURE_ACCESS_TOKEN). Managers are selected by the job's YAML; each one is
// a Provider here, and adding another is adding a file.
package secrets

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

// Env is the job's variable set (repo variables, CI_* and the OIDC token),
// which is where provider settings such as VAULT_ADDR come from, falling back
// to the runner process's environment.
type Env map[string]string

func (e Env) get(key string) string {
	if v, ok := e[key]; ok && v != "" {
		return v
	}
	return os.Getenv(key)
}

// Provider fetches one kind of secret.
type Provider interface {
	Name() string
	// Fetch returns the secret named by spec (which has this provider's
	// field set).
	Fetch(ctx context.Context, env Env, spec proto.SecretSpec) (string, error)
}

var providers = map[string]Provider{}

func register(p Provider) { providers[p.Name()] = p }

// Names lists the registered providers.
func Names() []string {
	out := make([]string, 0, len(providers))
	for n := range providers {
		out = append(out, n)
	}
	return out
}

// providerFor picks the provider a spec names.
func providerFor(spec proto.SecretSpec) (Provider, error) {
	var name string
	switch {
	case spec.Vault != "":
		name = "vault"
	case spec.AWS != "":
		name = "aws"
	case spec.GCP != "":
		name = "gcp"
	case spec.Azure != "":
		name = "azure"
	default:
		return nil, errors.New("secret names no provider")
	}
	p, ok := providers[name]
	if !ok {
		return nil, fmt.Errorf("secret provider %q is not built into this runner", name)
	}
	return p, nil
}

// Resolve fetches every secret a job declares. The returned map is
// name → value; the caller exposes them and masks the values in the log.
// One failure fails the lot: a job must never start with half its secrets.
func Resolve(ctx context.Context, env Env, specs map[string]proto.SecretSpec) (map[string]string, error) {
	out := map[string]string{}
	for name, spec := range specs {
		p, err := providerFor(spec)
		if err != nil {
			return nil, fmt.Errorf("secret %s: %w", name, err)
		}
		fctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		v, err := p.Fetch(fctx, env, spec)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("secret %s (%s): %w", name, p.Name(), err)
		}
		out[name] = v
	}
	return out, nil
}

// ---- shared HTTP plumbing ----

var httpClient = &http.Client{Timeout: 25 * time.Second}

// doJSON sends req and decodes a 2xx JSON body into v; a non-2xx status
// becomes an error carrying the first line of the body (never a secret —
// managers return errors, not values, with those statuses).
func doJSON(req *http.Request, v any) error {
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		line := strings.TrimSpace(string(body))
		if i := strings.IndexByte(line, '\n'); i >= 0 {
			line = line[:i]
		}
		if len(line) > 300 {
			line = line[:300] + "…"
		}
		return fmt.Errorf("%s %s: %s: %s", req.Method, req.URL.Host+req.URL.Path, resp.Status, line)
	}
	if v == nil {
		return nil
	}
	return jsonUnmarshal(body, v)
}

func postForm(ctx context.Context, endpoint string, form url.Values, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	return doJSON(req, v)
}

func getBearer(ctx context.Context, endpoint, token string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	return doJSON(req, v)
}

// jobToken is the job's OIDC token, "" when the server mints none.
func jobToken(env Env) string { return env["FORGE_OIDC_TOKEN"] }
