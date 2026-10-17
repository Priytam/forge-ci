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
)

// Registry login: a Docker config.json the runner prepares per job, merging
// the DOCKER_AUTH_CONFIG variable (any registry: Docker Hub, GHCR, Quay,
// Harbor, GitLab, Nexus...) with credentials minted for cloud registries
// from the job's identity — no long-lived registry passwords in Forge:
//
//	*.dkr.ecr.*.amazonaws.com   ECR GetAuthorizationToken (AWS_ROLE_ARN)
//	*.pkg.dev, gcr.io, *.gcr.io  Artifact Registry / GCR, access token (GCP_WORKLOAD_IDENTITY_PROVIDER)
//	*.azurecr.io                ACR token exchange (AZURE_TENANT_ID / AZURE_CLIENT_ID)

type dockerConfig struct {
	Auths map[string]dockerAuth `json:"auths"`
}

type dockerAuth struct {
	Auth string `json:"auth"`
}

// DockerConfig returns the config.json for a job: DOCKER_AUTH_CONFIG plus a
// login for each registry in logins. A registry without a cloud recipe must
// appear in DOCKER_AUTH_CONFIG. Returns nil when there is nothing to write.
func DockerConfig(ctx context.Context, env Env, logins []string) ([]byte, error) {
	cfg := dockerConfig{Auths: map[string]dockerAuth{}}
	if raw := env.get("DOCKER_AUTH_CONFIG"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
			return nil, fmt.Errorf("DOCKER_AUTH_CONFIG is not a Docker config.json: %w", err)
		}
		if cfg.Auths == nil {
			cfg.Auths = map[string]dockerAuth{}
		}
	}
	for _, registry := range logins {
		registry = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(registry, "https://"), "http://"))
		if registry == "" {
			continue
		}
		if _, have := cfg.Auths[registry]; have {
			continue // DOCKER_AUTH_CONFIG already covers it
		}
		user, pass, err := registryCredentials(ctx, env, registry)
		if err != nil {
			return nil, fmt.Errorf("registry %s: %w", registry, err)
		}
		cfg.Auths[registry] = dockerAuth{Auth: base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))}
	}
	if len(cfg.Auths) == 0 {
		return nil, nil
	}
	return json.MarshalIndent(cfg, "", "  ")
}

// RegistryKind names the cloud recipe for a registry host ("" = none).
func RegistryKind(registry string) string {
	switch {
	case strings.Contains(registry, ".dkr.ecr.") && strings.HasSuffix(registry, ".amazonaws.com"),
		strings.Contains(registry, ".dkr.ecr-fips."):
		return "ecr"
	case registry == "gcr.io", strings.HasSuffix(registry, ".gcr.io"), strings.HasSuffix(registry, ".pkg.dev"):
		return "gar"
	case strings.HasSuffix(registry, ".azurecr.io"), strings.HasSuffix(registry, ".azurecr.cn"), strings.HasSuffix(registry, ".azurecr.us"):
		return "acr"
	}
	return ""
}

func registryCredentials(ctx context.Context, env Env, registry string) (string, string, error) {
	switch RegistryKind(registry) {
	case "ecr":
		return ecrCredentials(ctx, env, registry)
	case "gar":
		tok, err := gcpAccessToken(ctx, env)
		if err != nil {
			return "", "", err
		}
		return "oauth2accesstoken", tok, nil
	case "acr":
		return acrCredentials(ctx, env, registry)
	}
	return "", "", errors.New("no cloud login recipe for this registry: put its credentials in the DOCKER_AUTH_CONFIG variable")
}

// acrCredentials exchanges an AAD token for an ACR refresh token, used as the
// password with ACR's well-known token username.
func acrCredentials(ctx context.Context, env Env, registry string) (string, string, error) {
	aad, err := azureAccessToken(ctx, env, "https://management.azure.com/.default")
	if err != nil {
		return "", "", err
	}
	base := env.get("ACR_EXCHANGE_ENDPOINT") // tests
	if base == "" {
		base = "https://" + registry
	}
	form := url.Values{"grant_type": {"access_token"}, "service": {registry}, "access_token": {aad}}
	if tenant := env.get("AZURE_TENANT_ID"); tenant != "" {
		form.Set("tenant", tenant)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/oauth2/exchange", strings.NewReader(form.Encode()))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var out struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := doJSON(req, &out); err != nil {
		return "", "", err
	}
	return "00000000-0000-0000-0000-000000000000", out.RefreshToken, nil
}
