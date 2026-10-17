package secrets

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

// azureProvider reads Azure Key Vault secrets.
//
//	secrets:
//	  CERT: {azure: "https://acme-ci.vault.azure.net/secrets/deploy-cert"}
//	  OLD:  {azure: "https://acme-ci.vault.azure.net/secrets/deploy-cert/7c1f..."}
//
// Identity: workload identity federation with the job's OIDC token —
// AZURE_TENANT_ID and AZURE_CLIENT_ID of an app registration (or managed
// identity) with a federated credential trusting Forge's issuer — else
// AZURE_ACCESS_TOKEN (the runner's own token for https://vault.azure.net).
type azureProvider struct{}

func init() { register(azureProvider{}) }

func (azureProvider) Name() string { return "azure" }

// azureAccessToken gets a token for scope (e.g. https://vault.azure.net/.default).
func azureAccessToken(ctx context.Context, env Env, scope string) (string, error) {
	if t := env.get("AZURE_ACCESS_TOKEN"); t != "" {
		return t, nil
	}
	tenant, client, jwt := env.get("AZURE_TENANT_ID"), env.get("AZURE_CLIENT_ID"), jobToken(env)
	if tenant == "" || client == "" || jwt == "" {
		return "", errors.New("set AZURE_TENANT_ID and AZURE_CLIENT_ID (and run with an OIDC token), or AZURE_ACCESS_TOKEN")
	}
	authority := strings.TrimSuffix(env.get("AZURE_AUTHORITY_HOST"), "/")
	if authority == "" {
		authority = "https://login.microsoftonline.com"
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := postForm(ctx, authority+"/"+url.PathEscape(tenant)+"/oauth2/v2.0/token", url.Values{
		"client_id":             {client},
		"grant_type":            {"client_credentials"},
		"scope":                 {scope},
		"client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"},
		"client_assertion":      {jwt},
	}, &out); err != nil {
		return "", fmt.Errorf("workload identity exchange: %w", err)
	}
	return out.AccessToken, nil
}

func (azureProvider) Fetch(ctx context.Context, env Env, spec proto.SecretSpec) (string, error) {
	u, err := url.Parse(spec.Azure)
	if err != nil || u.Scheme != "https" || !strings.HasPrefix(u.Path, "/secrets/") {
		return "", errors.New(`azure reference must be "https://<vault>.vault.azure.net/secrets/<name>[/<version>]"`)
	}
	token, err := azureAccessToken(ctx, env, "https://vault.azure.net/.default")
	if err != nil {
		return "", err
	}
	var out struct {
		Value string `json:"value"`
	}
	if err := getBearer(ctx, u.Scheme+"://"+u.Host+strings.TrimSuffix(u.Path, "/")+"?api-version=7.4", token, &out); err != nil {
		return "", err
	}
	return out.Value, nil
}
