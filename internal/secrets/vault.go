package secrets

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

// vaultProvider reads HashiCorp Vault (or OpenBao) KV v2 secrets.
//
//	secrets:
//	  DB_PASSWORD: {vault: "ci/prod/db#password"}         # mount "secret"
//	  TLS_KEY:     {vault: "ci/prod/tls#key@kv", file: true}
//
// Settings (job variables, else runner env): VAULT_ADDR (required),
// VAULT_NAMESPACE (Enterprise), and either VAULT_TOKEN, or JWT auth with the
// job's OIDC token: VAULT_AUTH_ROLE (required) and VAULT_AUTH_PATH (mount of
// the jwt auth method, default "jwt").
type vaultProvider struct{}

func init() { register(vaultProvider{}) }

func (vaultProvider) Name() string { return "vault" }

// parseVaultRef splits "path#field@mount" (mount optional, default secret).
func parseVaultRef(ref string) (mount, path, field string, err error) {
	mount = "secret"
	rest := ref
	if i := strings.LastIndex(rest, "@"); i >= 0 {
		mount, rest = rest[i+1:], rest[:i]
	}
	path, field, ok := strings.Cut(rest, "#")
	path = strings.Trim(path, "/")
	if !ok || path == "" || field == "" || mount == "" {
		return "", "", "", errors.New(`vault reference must be "path#field" or "path#field@mount"`)
	}
	return mount, path, field, nil
}

// vaultToken returns a token for the job: VAULT_TOKEN, else a JWT login.
func vaultToken(ctx context.Context, env Env, addr string) (string, error) {
	if t := env.get("VAULT_TOKEN"); t != "" {
		return t, nil
	}
	jwt := jobToken(env)
	role := env.get("VAULT_AUTH_ROLE")
	if jwt == "" || role == "" {
		return "", errors.New("no VAULT_TOKEN, and JWT auth needs the job's OIDC token plus VAULT_AUTH_ROLE")
	}
	mount := env.get("VAULT_AUTH_PATH")
	if mount == "" {
		mount = "jwt"
	}
	body, _ := json.Marshal(map[string]string{"role": role, "jwt": jwt})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, addr+"/v1/auth/"+strings.Trim(mount, "/")+"/login", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if ns := env.get("VAULT_NAMESPACE"); ns != "" {
		req.Header.Set("X-Vault-Namespace", ns)
	}
	var out struct {
		Auth struct {
			ClientToken string `json:"client_token"`
		} `json:"auth"`
	}
	if err := doJSON(req, &out); err != nil {
		return "", fmt.Errorf("jwt login: %w", err)
	}
	if out.Auth.ClientToken == "" {
		return "", errors.New("jwt login returned no token")
	}
	return out.Auth.ClientToken, nil
}

func (vaultProvider) Fetch(ctx context.Context, env Env, spec proto.SecretSpec) (string, error) {
	addr := strings.TrimSuffix(env.get("VAULT_ADDR"), "/")
	if addr == "" {
		return "", errors.New("VAULT_ADDR is not set")
	}
	mount, path, field, err := parseVaultRef(spec.Vault)
	if err != nil {
		return "", err
	}
	token, err := vaultToken(ctx, env, addr)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, addr+"/v1/"+mount+"/data/"+path, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-Vault-Token", token)
	if ns := env.get("VAULT_NAMESPACE"); ns != "" {
		req.Header.Set("X-Vault-Namespace", ns)
	}
	var out struct {
		Data struct {
			Data map[string]any `json:"data"`
		} `json:"data"`
	}
	if err := doJSON(req, &out); err != nil {
		return "", err
	}
	v, ok := out.Data.Data[field]
	if !ok {
		return "", fmt.Errorf("field %q not in %s/%s", field, mount, path)
	}
	if s, ok := v.(string); ok {
		return s, nil
	}
	raw, _ := json.Marshal(v)
	return string(raw), nil
}
