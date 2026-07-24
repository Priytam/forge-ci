package store

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

// Throwaway RSA key generated locally with openssl for this test ONLY (not a
// real GitHub App key): openssl genrsa | openssl rsa -traditional.
const appTestKey = `-----BEGIN RSA PRIVATE KEY-----
MIIEowIBAAKCAQEAqtCg/eSa3VqcLmLx+V2aTLfv4ujDCP45y43zFvWrKRe3E3jb
miINM3FveDAMdyZSD4Der9fIlXthRSOG2vwhY2Qa1LmSqe1YKfPdIQW6hATJUeQp
oTYPqtcAMeUEXUVlaEUWX9e9LuQrdgbVtsFUL0JtksiUqiL+s78JxXd5HHoMIb89
icRD0sLx39OuNsIiQCkbS/glD8iasrm+yojvUZ4CSJcclBEw1zTq5uja2GnKPwj7
wKKo9gjEM44BG4N2ILQi6GMt7cJbIWUZR+5/Imm1wJygGO9qMT1ViI6J/+BBr2Ia
NfPWeqGhnZg3jCRNmEbmkwy9YQzW6KyK8yUQAQIDAQABAoIBAE6DedRpvQMssGgj
47wumYtU6ob+XRNno1Ica5Vsk2FefLCPF0V4DGBObiGs2DX2H7bvkav6v8Bxxyp7
43MJfCFOtIR9zducdC9IX6ZblzkyaATjnnzyt+3bSEQm08Q5bxyn0Np59AO3LgDg
sGAB6fuVCX90Ad1YG4GsOEYTHdCHykXHuxsC/rn6xHuHzIA17V2V96T0oF0pn85P
CA7DPJmI244TTS7bVs1ad1yqGhotBLRtVOeR3hrDlAdMwCN/3y4PfXmu4qQEIYSi
TcSEXIVcBCmBOc/qBg1Ud+2q5nDJCUp/GLJVARwCPjAyEeEIydikVH/N3m42deMV
mF0hvscCgYEA3GlyGr5j7W8pl8N/J73i5UkUdh7eDfyrVTdtAymhqahO7+bMCBBp
A0RNlt6ieWOLp5mg1o4D1u6n0ch8W2HeWkq0UgHWtSV8FHrarm4cuink8D8f1CcH
dnmasvgTtaiolFY5bXnFjovDiv6SXapFxoyaKAycyqQ/HA3P10PLWkMCgYEAxmUh
15CjnepmXufLlScpOEllpkcBv4bG5yi209M4OWvjLiWMub348NN1hDvdTPOu7HyS
auKRYPljjJin7Jc9Qvra7KRX8NXHw9iRVxKNpF+cniDnmv9dVfNzY/f4Sz1QkXKq
6h+eRdEezTaFTfBpqdr9kQuLNpD+3dNDxkkx8msCgYEAz2xgWHCyA6EoaE0vXcwi
OhrDKcI0wL720jRd36sPG2VsG/J8Ml6XJN7jkcak6k3XAHvgU+nEDUH9Jrxg43K/
2QMSnVZjo4fKNE/Fen/fgwaoD7uoDXRJXqJkBmbVzZASTb6zPqZpV5OKC0U1ovX5
wjdRX7021LErPxB0dyWyupcCgYB3OuwduuU50Fb5jmCBIOna0/Fs/puEWSFMZuGJ
aBUQHVCIuTRbpFnpkYu8jqWuy3xCz5LG/abVGsvDATNaMoI0sMHFGfdn23KUtqCS
LapGMNfVCH1oXzPepdKhL7NetFipMLqavanG16ilN7DhaCx4Ug21j7R4dKdW9NJ0
ZiTIRQKBgCVqIl5tcwFt5d2qAXRti5rGbmPRm1nBsoHz22OXhyaMyvnl4XnFIKoG
ZwV1RurNMzf9Ux3HT+zLP9H/gBEgzbCmI4Q5veJqBivjKw4DJiH+dtFiqIR61/VY
ziDlARvlzyaW2kTcihSDOvYGPQYyJ6G7thkmxe7GeMlQr3cqV324
-----END RSA PRIVATE KEY-----`

func appBaseDSN() string {
	if v := os.Getenv("DATABASE_URL"); v != "" {
		return v
	}
	return "postgres://forge:forge@localhost:5433/forge?sslmode=disable"
}

// newAppTestStore creates a throwaway DB with GITHUB_API_BASE pointed at
// stubURL and FORGE_SECRET_KEY set (so the App private key is encrypted at
// rest). Both env vars are set BEFORE store.New so the Store's minter and cipher
// pick them up. The DB is dropped on cleanup.
func newAppTestStore(t *testing.T, stubURL string) *Store {
	t.Helper()
	t.Setenv("GITHUB_API_BASE", stubURL)
	// A deterministic 32-byte base64 key ("AAA...=" is 32 zero bytes).
	t.Setenv("FORGE_SECRET_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")

	ctx := context.Background()
	admin, err := pgx.Connect(ctx, appBaseDSN())
	if err != nil {
		t.Skipf("postgres not reachable (%v); skipping DB-backed app-auth test", err)
	}
	dbName := fmt.Sprintf("forge_app_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		admin.Close(ctx)
		t.Skipf("cannot create throwaway db (%v); skipping", err)
	}
	u, _ := url.Parse(appBaseDSN())
	u.Path = "/" + dbName
	st, err := New(ctx, u.String())
	if err != nil {
		_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName+" WITH (FORCE)")
		admin.Close(ctx)
		t.Fatalf("store.New on throwaway db: %v", err)
	}
	t.Cleanup(func() {
		st.Close()
		_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName+" WITH (FORCE)")
		admin.Close(ctx)
	})
	return st
}

type appMintStub struct {
	mu     sync.Mutex
	hits   int
	tokenN int
}

func newAppMintStub() (*appMintStub, *httptest.Server) {
	s := &appMintStub{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.hits++
		s.tokenN++
		tok := fmt.Sprintf("ghs_minted_%d", s.tokenN)
		s.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      tok,
			"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
		})
	}))
	return s, srv
}

func (s *appMintStub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits
}

// TestAppRepoResolvesMintedToken registers an App-authed repo pointed at a mint
// stub and asserts cloneAuth and RepoStatusTarget both yield the stub-minted
// token (embedded in the clone URL / returned for the Authorization header),
// that the minted token is cached across both resolves (stub hit once), and that
// the private key is encrypted at rest and never returned by ListRegisteredRepos.
func TestAppRepoResolvesMintedToken(t *testing.T) {
	ctx := context.Background()
	stub, srv := newAppMintStub()
	defer srv.Close()
	st := newAppTestStore(t, srv.URL)

	const repo = "acme/app-authed"
	if err := st.RegisterRepo(ctx, proto.RepoRegistration{
		Repo:                 repo,
		Provider:             "github",
		CloneURL:             "https://github.com/" + repo + ".git",
		DefaultBranch:        "main",
		GitHubAppID:          "111111",
		GitHubInstallationID: "222222",
		GitHubAppPrivateKey:  appTestKey,
	}); err != nil {
		t.Fatalf("RegisterRepo (app): %v", err)
	}

	// The private key is encrypted at rest.
	var storedKey string
	if err := st.pool.QueryRow(ctx,
		`SELECT github_app_private_key FROM repo_registry WHERE repo=$1`, repo).Scan(&storedKey); err != nil {
		t.Fatalf("read stored key: %v", err)
	}
	if !strings.HasPrefix(storedKey, "enc:v1:") {
		t.Fatalf("private key not encrypted at rest: %.16q...", storedKey)
	}
	if strings.Contains(storedKey, "PRIVATE KEY") {
		t.Fatal("private key stored in plaintext")
	}

	// cloneAuth mints and embeds the token.
	cloneURL, token, err := st.cloneAuth(ctx, repo)
	if err != nil {
		t.Fatalf("cloneAuth: %v", err)
	}
	if token != "ghs_minted_1" {
		t.Errorf("cloneAuth token = %q, want minted ghs_minted_1", token)
	}
	if !strings.Contains(cloneURL, "x-access-token:ghs_minted_1@github.com") {
		t.Errorf("clone URL missing minted token: %q", cloneURL)
	}

	// RepoStatusTarget resolves the same (cached) minted token — no second mint.
	provider, statusTok, ok, err := st.RepoStatusTarget(ctx, repo)
	if err != nil || !ok {
		t.Fatalf("RepoStatusTarget: ok=%v err=%v", ok, err)
	}
	if provider != "github" {
		t.Errorf("provider = %q, want github", provider)
	}
	if statusTok != "ghs_minted_1" {
		t.Errorf("status token = %q, want cached ghs_minted_1", statusTok)
	}
	if n := stub.count(); n != 1 {
		t.Fatalf("mint stub hit %d times, want exactly 1 (token cached across both resolves)", n)
	}

	// ListRegisteredRepos never returns the private key.
	repos, err := st.ListRegisteredRepos(ctx)
	if err != nil {
		t.Fatalf("ListRegisteredRepos: %v", err)
	}
	var found bool
	for _, r := range repos {
		if r.Repo != repo {
			continue
		}
		found = true
		if r.GitHubAppPrivateKey != "" {
			t.Error("ListRegisteredRepos returned the private key")
		}
		if !r.HasGitHubApp {
			t.Error("HasGitHubApp = false, want true")
		}
		if r.GitHubAppID != "111111" || r.GitHubInstallationID != "222222" {
			t.Errorf("app id/installation not surfaced: id=%q inst=%q", r.GitHubAppID, r.GitHubInstallationID)
		}
		if r.HasToken {
			t.Error("HasToken = true for an App-only repo")
		}
	}
	if !found {
		t.Fatal("registered app repo not in ListRegisteredRepos")
	}
}

// TestPATRepoUnaffected asserts a static-token repo resolves its PAT unchanged
// and never triggers a GitHub App mint.
func TestPATRepoUnaffected(t *testing.T) {
	ctx := context.Background()
	stub, srv := newAppMintStub()
	defer srv.Close()
	st := newAppTestStore(t, srv.URL)

	const repo, pat = "acme/pat-authed", "ghp_static_pat_value"
	if err := st.RegisterRepo(ctx, proto.RepoRegistration{
		Repo:          repo,
		Provider:      "github",
		CloneURL:      "https://github.com/" + repo + ".git",
		DefaultBranch: "main",
		Token:         pat,
	}); err != nil {
		t.Fatalf("RegisterRepo (pat): %v", err)
	}

	cloneURL, token, err := st.cloneAuth(ctx, repo)
	if err != nil {
		t.Fatalf("cloneAuth: %v", err)
	}
	if token != pat {
		t.Errorf("token = %q, want the PAT unchanged", token)
	}
	if !strings.Contains(cloneURL, "x-access-token:"+pat+"@github.com") {
		t.Errorf("clone URL missing PAT: %q", cloneURL)
	}

	provider, statusTok, ok, err := st.RepoStatusTarget(ctx, repo)
	if err != nil || !ok {
		t.Fatalf("RepoStatusTarget: ok=%v err=%v", ok, err)
	}
	if provider != "github" || statusTok != pat {
		t.Errorf("status target = (%q,%q), want (github, PAT)", provider, statusTok)
	}

	if n := stub.count(); n != 0 {
		t.Fatalf("mint stub hit %d times for a PAT repo, want 0", n)
	}

	repos, err := st.ListRegisteredRepos(ctx)
	if err != nil {
		t.Fatalf("ListRegisteredRepos: %v", err)
	}
	for _, r := range repos {
		if r.Repo == repo {
			if !r.HasToken {
				t.Error("HasToken = false for a PAT repo")
			}
			if r.HasGitHubApp {
				t.Error("HasGitHubApp = true for a PAT repo")
			}
		}
	}
}
