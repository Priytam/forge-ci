package secrets

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

const jobJWT = "eyJhbGciOiJSUzI1NiJ9.e30.sig"

func TestVaultJWTLoginAndRead(t *testing.T) {
	var sawJWT, sawToken, sawNS string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/auth/forge/login":
			var in map[string]string
			_ = json.NewDecoder(r.Body).Decode(&in)
			sawJWT = in["jwt"]
			if in["role"] != "ci" {
				w.WriteHeader(403)
				return
			}
			_, _ = w.Write([]byte(`{"auth":{"client_token":"hvs.abc"}}`))
		case "/v1/kv/data/ci/prod/db":
			sawToken, sawNS = r.Header.Get("X-Vault-Token"), r.Header.Get("X-Vault-Namespace")
			_, _ = w.Write([]byte(`{"data":{"data":{"password":"p@ss","port":5432}}}`))
		default:
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"errors":["no handler"]}`))
		}
	}))
	defer srv.Close()
	env := Env{"VAULT_ADDR": srv.URL, "VAULT_AUTH_ROLE": "ci", "VAULT_AUTH_PATH": "forge", "VAULT_NAMESPACE": "team-a", "FORGE_OIDC_TOKEN": jobJWT}
	got, err := Resolve(context.Background(), env, map[string]proto.SecretSpec{
		"DB_PASSWORD": {Vault: "ci/prod/db#password@kv"},
		"DB_PORT":     {Vault: "ci/prod/db#port@kv"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got["DB_PASSWORD"] != "p@ss" || got["DB_PORT"] != "5432" || sawJWT != jobJWT || sawToken != "hvs.abc" || sawNS != "team-a" {
		t.Errorf("got %v (jwt %q token %q ns %q)", got, sawJWT, sawToken, sawNS)
	}
	if _, err := Resolve(context.Background(), env, map[string]proto.SecretSpec{"X": {Vault: "ci/prod/db#nope@kv"}}); err == nil || !strings.Contains(err.Error(), `field "nope"`) {
		t.Errorf("missing field: %v", err)
	}
	if _, err := Resolve(context.Background(), env, map[string]proto.SecretSpec{"X": {Vault: "missing#f@kv"}}); err == nil || !strings.Contains(err.Error(), "404") || strings.Contains(err.Error(), "hvs.abc") {
		t.Errorf("404 error = %v (must not leak the token)", err)
	}
	// Without a token or role: a clear error, no request.
	if _, err := Resolve(context.Background(), Env{"VAULT_ADDR": srv.URL}, map[string]proto.SecretSpec{"X": {Vault: "a#b"}}); err == nil || !strings.Contains(err.Error(), "VAULT_AUTH_ROLE") {
		t.Errorf("no auth: %v", err)
	}
}

func TestGCPWorkloadIdentityAndSecret(t *testing.T) {
	var audience, subject, bearer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/token":
			_ = r.ParseForm()
			audience, subject = r.Form.Get("audience"), r.Form.Get("subject_token")
			_, _ = w.Write([]byte(`{"access_token":"ya29.federated"}`))
		case strings.HasSuffix(r.URL.Path, ":generateAccessToken"):
			if r.Header.Get("Authorization") != "Bearer ya29.federated" {
				w.WriteHeader(401)
				return
			}
			_, _ = w.Write([]byte(`{"accessToken":"ya29.impersonated"}`))
		case strings.HasSuffix(r.URL.Path, ":access"):
			bearer = r.Header.Get("Authorization")
			if !strings.Contains(r.URL.Path, "/projects/my-proj/secrets/api-token/versions/latest:access") {
				w.WriteHeader(404)
				return
			}
			_, _ = w.Write([]byte(`{"payload":{"data":"` + base64.StdEncoding.EncodeToString([]byte("tok-123")) + `"}}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	env := Env{
		"FORGE_OIDC_TOKEN": jobJWT, "GCP_PROJECT": "my-proj",
		"GCP_WORKLOAD_IDENTITY_PROVIDER": "//iam.googleapis.com/projects/1/locations/global/workloadIdentityPools/forge/providers/forge",
		"GCP_SERVICE_ACCOUNT":            "deployer@my-proj.iam.gserviceaccount.com",
		"GCP_STS_ENDPOINT":               srv.URL, "GCP_IAM_CREDENTIALS_ENDPOINT": srv.URL, "GCP_SECRET_MANAGER_ENDPOINT": srv.URL,
	}
	got, err := Resolve(context.Background(), env, map[string]proto.SecretSpec{"TOKEN": {GCP: "api-token"}})
	if err != nil || got["TOKEN"] != "tok-123" {
		t.Fatalf("got %v %v", got, err)
	}
	if !strings.HasPrefix(audience, "//iam.googleapis.com/") || subject != jobJWT || bearer != "Bearer ya29.impersonated" {
		t.Errorf("exchange = aud %q subject %q bearer %q", audience, subject, bearer)
	}
	// Registry login for Artifact Registry reuses the same token.
	cfg, err := DockerConfig(context.Background(), env, []string{"europe-docker.pkg.dev"})
	if err != nil {
		t.Fatal(err)
	}
	var parsed dockerConfig
	_ = json.Unmarshal(cfg, &parsed)
	raw, _ := base64.StdEncoding.DecodeString(parsed.Auths["europe-docker.pkg.dev"].Auth)
	if string(raw) != "oauth2accesstoken:ya29.impersonated" {
		t.Errorf("GAR auth = %q", raw)
	}
}

func TestAzureKeyVaultAndACR(t *testing.T) {
	var assertion, scope string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/oauth2/v2.0/token"):
			_ = r.ParseForm()
			assertion, scope = r.Form.Get("client_assertion"), r.Form.Get("scope")
			_, _ = w.Write([]byte(`{"access_token":"aad-token"}`))
		case r.URL.Path == "/secrets/deploy-cert":
			if r.Header.Get("Authorization") != "Bearer aad-token" || r.URL.Query().Get("api-version") == "" {
				w.WriteHeader(401)
				return
			}
			_, _ = w.Write([]byte(`{"value":"-----BEGIN CERT-----"}`))
		case r.URL.Path == "/oauth2/exchange":
			_ = r.ParseForm()
			if r.Form.Get("access_token") != "aad-token" || r.Form.Get("service") != "acme.azurecr.io" {
				w.WriteHeader(401)
				return
			}
			_, _ = w.Write([]byte(`{"refresh_token":"acr-refresh"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	env := Env{"FORGE_OIDC_TOKEN": jobJWT, "AZURE_TENANT_ID": "t1", "AZURE_CLIENT_ID": "c1", "AZURE_AUTHORITY_HOST": srv.URL, "ACR_EXCHANGE_ENDPOINT": srv.URL}
	// Key Vault URLs are https in real life; the test vault is this server.
	kv := azureProvider{}
	_, err := kv.Fetch(context.Background(), env, proto.SecretSpec{Azure: "http://" + strings.TrimPrefix(srv.URL, "http://") + "/secrets/deploy-cert"})
	if err == nil || !strings.Contains(err.Error(), "https") {
		t.Errorf("http vault URL accepted: %v", err)
	}
	tok, err := azureAccessToken(context.Background(), env, "https://vault.azure.net/.default")
	if err != nil || tok != "aad-token" || assertion != jobJWT || scope != "https://vault.azure.net/.default" {
		t.Fatalf("token = %q %v (assertion %q scope %q)", tok, err, assertion, scope)
	}
	user, pass, err := acrCredentials(context.Background(), env, "acme.azurecr.io")
	if err != nil || user != "00000000-0000-0000-0000-000000000000" || pass != "acr-refresh" {
		t.Errorf("acr = %q %q %v", user, pass, err)
	}
}

func TestDockerConfigMergesAndRefusesUnknown(t *testing.T) {
	env := Env{"DOCKER_AUTH_CONFIG": `{"auths":{"ghcr.io":{"auth":"dXNlcjpwYXNz"}}}`}
	cfg, err := DockerConfig(context.Background(), env, []string{"ghcr.io"})
	if err != nil || !strings.Contains(string(cfg), "ghcr.io") {
		t.Fatalf("cfg = %s %v", cfg, err)
	}
	if _, err := DockerConfig(context.Background(), env, []string{"registry.example.com"}); err == nil || !strings.Contains(err.Error(), "DOCKER_AUTH_CONFIG") {
		t.Errorf("unknown registry: %v", err)
	}
	if c, err := DockerConfig(context.Background(), Env{}, nil); c != nil || err != nil {
		t.Errorf("nothing to do = %s %v", c, err)
	}
	for host, want := range map[string]string{
		"123456789012.dkr.ecr.eu-west-1.amazonaws.com": "ecr", "us-docker.pkg.dev": "gar", "gcr.io": "gar",
		"acme.azurecr.io": "acr", "docker.io": "", "harbor.acme.io": "",
	} {
		if got := RegistryKind(host); got != want {
			t.Errorf("RegistryKind(%s) = %q, want %q", host, got, want)
		}
	}
}

func TestRefParsing(t *testing.T) {
	m, p, f, err := parseVaultRef("ci/prod/db#password@kv")
	if err != nil || m != "kv" || p != "ci/prod/db" || f != "password" {
		t.Errorf("vault ref = %s %s %s %v", m, p, f, err)
	}
	if _, _, _, err := parseVaultRef("nofield"); err == nil {
		t.Error("vault ref without field accepted")
	}
	n, err := gcpSecretName(Env{"GCP_PROJECT": "p"}, "s")
	if err != nil || n != "projects/p/secrets/s/versions/latest" {
		t.Errorf("gcp name = %s %v", n, err)
	}
	if _, err := gcpSecretName(Env{}, "s"); err == nil {
		t.Error("short gcp name without project accepted")
	}
}
