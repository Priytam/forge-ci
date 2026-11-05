package secrets

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

// gcpProvider reads Google Secret Manager.
//
//	secrets:
//	  SA_KEY: {gcp: "projects/my-proj/secrets/deployer-key", file: true}
//	  TOKEN:  {gcp: "api-token"}                        # needs GCP_PROJECT
//	  OLD:    {gcp: "projects/p/secrets/s/versions/3"}
//
// Identity: workload identity federation with the job's OIDC token —
// GCP_WORKLOAD_IDENTITY_PROVIDER (the full
// //iam.googleapis.com/projects/N/locations/global/workloadIdentityPools/P/providers/X
// name) and optionally GCP_SERVICE_ACCOUNT to impersonate — else
// GOOGLE_OAUTH_ACCESS_TOKEN (the runner's own token).
type gcpProvider struct{}

func init() { register(gcpProvider{}) }

func (gcpProvider) Name() string { return "gcp" }

func gcpEndpoint(env Env, key, def string) string {
	if v := env.get(key); v != "" {
		return strings.TrimSuffix(v, "/")
	}
	return def
}

// gcpAccessToken exchanges the job's OIDC token for a Google access token.
func gcpAccessToken(ctx context.Context, env Env) (string, error) {
	if t := env.get("GOOGLE_OAUTH_ACCESS_TOKEN"); t != "" {
		return t, nil
	}
	provider := env.get("GCP_WORKLOAD_IDENTITY_PROVIDER")
	jwt := jobToken(env)
	if provider == "" || jwt == "" {
		return "", errors.New("set GCP_WORKLOAD_IDENTITY_PROVIDER (and run with an OIDC token), or GOOGLE_OAUTH_ACCESS_TOKEN")
	}
	var sts struct {
		AccessToken string `json:"access_token"`
	}
	if err := postForm(ctx, gcpEndpoint(env, "GCP_STS_ENDPOINT", "https://sts.googleapis.com")+"/v1/token", url.Values{
		"audience":             {provider},
		"grant_type":           {"urn:ietf:params:oauth:grant-type:token-exchange"},
		"requested_token_type": {"urn:ietf:params:oauth:token-type:access_token"},
		"subject_token_type":   {"urn:ietf:params:oauth:token-type:jwt"},
		"subject_token":        {jwt},
		"scope":                {"https://www.googleapis.com/auth/cloud-platform"},
	}, &sts); err != nil {
		return "", fmt.Errorf("workload identity exchange: %w", err)
	}
	sa := env.get("GCP_SERVICE_ACCOUNT")
	if sa == "" {
		return sts.AccessToken, nil // direct resource access (no impersonation)
	}
	body, _ := json.Marshal(map[string]any{"scope": []string{"https://www.googleapis.com/auth/cloud-platform"}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		gcpEndpoint(env, "GCP_IAM_CREDENTIALS_ENDPOINT", "https://iamcredentials.googleapis.com")+
			"/v1/projects/-/serviceAccounts/"+url.PathEscape(sa)+":generateAccessToken", strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+sts.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	var out struct {
		AccessToken string `json:"accessToken"`
	}
	if err := doJSON(req, &out); err != nil {
		return "", fmt.Errorf("impersonating %s: %w", sa, err)
	}
	return out.AccessToken, nil
}

// gcpSecretName expands a short name to projects/P/secrets/S/versions/latest.
func gcpSecretName(env Env, ref string) (string, error) {
	name := strings.Trim(ref, "/")
	if !strings.HasPrefix(name, "projects/") {
		project := env.get("GCP_PROJECT")
		if project == "" {
			project = env.get("GOOGLE_CLOUD_PROJECT")
		}
		if project == "" {
			return "", errors.New(`a short secret name needs GCP_PROJECT, or use "projects/<p>/secrets/<name>"`)
		}
		name = "projects/" + project + "/secrets/" + name
	}
	if !strings.Contains(name, "/versions/") {
		name += "/versions/latest"
	}
	return name, nil
}

func (gcpProvider) Fetch(ctx context.Context, env Env, spec proto.SecretSpec) (string, error) {
	name, err := gcpSecretName(env, spec.GCP)
	if err != nil {
		return "", err
	}
	token, err := gcpAccessToken(ctx, env)
	if err != nil {
		return "", err
	}
	var out struct {
		Payload struct {
			Data string `json:"data"`
		} `json:"payload"`
	}
	if err := getBearer(ctx, gcpEndpoint(env, "GCP_SECRET_MANAGER_ENDPOINT", "https://secretmanager.googleapis.com")+"/v1/"+name+":access", token, &out); err != nil {
		return "", err
	}
	raw, err := base64.StdEncoding.DecodeString(out.Payload.Data)
	if err != nil {
		return "", errors.New("secret payload is not base64")
	}
	return string(raw), nil
}
