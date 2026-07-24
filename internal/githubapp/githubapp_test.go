package githubapp

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Throwaway RSA keys generated locally with openssl for these tests ONLY — NOT
// a real GitHub App key:
//
//	openssl genrsa -out k.pem 2048
//	openssl rsa -traditional -in k.pem -out pkcs1.pem   # BEGIN RSA PRIVATE KEY
//	openssl pkcs8 -topk8 -nocrypt -in k.pem -out pkcs8.pem  # BEGIN PRIVATE KEY
const testKeyPKCS1 = `-----BEGIN RSA PRIVATE KEY-----
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

const testKeyPKCS8 = `-----BEGIN PRIVATE KEY-----
MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQCq0KD95JrdWpwu
YvH5XZpMt+/i6MMI/jnLjfMW9aspF7cTeNuaIg0zcW94MAx3JlIPgN6v18iVe2FF
I4ba/CFjZBrUuZKp7Vgp890hBbqEBMlR5CmhNg+q1wAx5QRdRWVoRRZf170u5Ct2
BtW2wVQvQm2SyJSqIv6zvwnFd3kcegwhvz2JxEPSwvHf0642wiJAKRtL+CUPyJqy
ub7KiO9RngJIlxyUETDXNOrm6NrYaco/CPvAoqj2CMQzjgEbg3YgtCLoYy3twlsh
ZRlH7n8iabXAnKAY72oxPVWIjon/4EGvYho189Z6oaGdmDeMJE2YRuaTDL1hDNbo
rIrzJRABAgMBAAECggEAToN51Gm9AyywaCPjvC6Zi1Tqhv5dE2ejUhxrlWyTYV58
sI8XRXgMYE5uIazYNfYftu+Rq/q/wHHHKnvjcwl8IU60hH3N25x0L0hfpluXOTJo
BOOefPK37dtIRCbTxDlvHKfQ2nn0A7cuAOCwYAHp+5UJf3QB3Vgbgaw4RhMd0IfK
Rce7GwL+ufrEe4fMgDXtXZX3pPSgXSmfzk8IDsM8mYjbjhNNLttWzVp3XKoaGi0E
tG1U55HeGsOUB0zAI3/fLg99ea7ipAQhhKJNxIRchVwEKYE5z+oGDVR37armcMkJ
Sn8YslUBHAI+MDIR4QjJ2KRUf83ebjZ14xWYXSG+xwKBgQDcaXIavmPtbymXw38n
veLlSRR2Ht4N/KtVN20DKaGpqE7v5swIEGkDRE2W3qJ5Y4unmaDWjgPW7qfRyHxb
Yd5aSrRSAda1JXwUetqubhy6KeTwPx/UJwd2eZqy+BO1qKiUVjltecWOi8OK/pJd
qkXGjJooDJzKpD8cDc/XQ8taQwKBgQDGZSHXkKOd6mZe58uVJyk4SWWmRwG/hsbn
KLbT0zg5a+MuJYy5vfjw03WEO91M867sfJJq4pFg+WOMmKfslz1C+trspFfw1cfD
2JFXEo2kX5yeIOea/11V83Nj9/hLPVCRcqrqH55F0R7NNoVN8Gmp2v2RC4s2kP7d
00PGSTHyawKBgQDPbGBYcLIDoShoTS9dzCI6GsMpwjTAvvbSNF3fqw8bZWwb8nwy
Xpck3uORxqTqTdcAe+BT6cQNQf0mvGDjcr/ZAxKdVmOjh8o0T8V6f9+DBqgPu6gN
dEleomQGZtXNkBJNvrM+pmlXk4oLRTWi9fnCN1FfvTbUsSs/EHR3JbK6lwKBgHc6
7B265TnQVvmOYIEg6drT8Wz+m4RZIUxm4YloFRAdUIi5NFukWemRi7yOpa7LfELP
ksb9ptUay8MBM1oygjSwwcUZ92fbcpS2oJItqkYw19UIfWhfM96l0qEvs160WKkw
upq9qcbXqKU3sOFoLHhSDbWPtHh0p1b00nRmJMhFAoGAJWoiXm1zAW3l3aoBdG2L
msZuY9GbWcGygfPbY5eHJozK+eXhecUgqgZnBXVG6s0zN/1THcdP7Ms/0f+AESDN
sKYjhDm94moGK+MrDgMmIf520WKohHrX9VjOIOUBG+XPJpbaRNyKFIM69gY9BjIn
obu2GSbF7sZ4yVCvdypXfbg=
-----END PRIVATE KEY-----`

// TestParseRSAPrivateKey verifies both PKCS#1 and PKCS#8 PEM are accepted and
// that garbage is rejected without leaking key bytes.
func TestParseRSAPrivateKey(t *testing.T) {
	for name, pem := range map[string]string{"pkcs1": testKeyPKCS1, "pkcs8": testKeyPKCS8} {
		k, err := parseRSAPrivateKey(pem)
		if err != nil {
			t.Fatalf("%s: parse: %v", name, err)
		}
		if k == nil {
			t.Fatalf("%s: nil key", name)
		}
	}
	if _, err := parseRSAPrivateKey("not a pem"); err == nil {
		t.Fatal("expected error for non-PEM input")
	}
}

// TestSignJWTClaims signs a JWT and asserts the RS256 header, the iss/iat/exp
// claims, and that the signature verifies against the key's public half.
func TestSignJWTClaims(t *testing.T) {
	key, err := parseRSAPrivateKey(testKeyPKCS1)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	tok, err := signJWT("123456", key, now)
	if err != nil {
		t.Fatalf("signJWT: %v", err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT should have 3 parts, got %d", len(parts))
	}

	// Header.
	var header map[string]string
	decodeSeg(t, parts[0], &header)
	if header["alg"] != "RS256" {
		t.Errorf("alg = %q, want RS256", header["alg"])
	}
	if header["typ"] != "JWT" {
		t.Errorf("typ = %q, want JWT", header["typ"])
	}

	// Claims.
	var claims struct {
		Iss string `json:"iss"`
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
	}
	decodeSeg(t, parts[1], &claims)
	if claims.Iss != "123456" {
		t.Errorf("iss = %q, want 123456", claims.Iss)
	}
	if want := now.Add(-60 * time.Second).Unix(); claims.Iat != want {
		t.Errorf("iat = %d, want %d (now-60s)", claims.Iat, want)
	}
	if want := now.Add(jwtTTL).Unix(); claims.Exp != want {
		t.Errorf("exp = %d, want %d (now+jwtTTL)", claims.Exp, want)
	}
	if claims.Exp-claims.Iat > int64((10 * time.Minute).Seconds()) {
		t.Errorf("exp-iat = %ds exceeds GitHub's 10 minute cap", claims.Exp-claims.Iat)
	}

	// Signature verifies against the public key.
	signingInput := parts[0] + "." + parts[1]
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	digest := sha256.Sum256([]byte(signingInput))
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], sig); err != nil {
		t.Fatalf("signature does not verify: %v", err)
	}
}

func decodeSeg(t *testing.T, seg string, v any) {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		t.Fatalf("base64url decode: %v", err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("json decode: %v", err)
	}
}

// mintStub records installation-token requests and returns a token + a
// test-controlled expiry.
type mintStub struct {
	mu      sync.Mutex
	hits    int
	auths   []string
	paths   []string
	accepts []string
	expiry  time.Time
	tokenN  int
}

func newMintStub() (*mintStub, *httptest.Server) {
	s := &mintStub{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.hits++
		s.tokenN++
		s.auths = append(s.auths, r.Header.Get("Authorization"))
		s.paths = append(s.paths, r.URL.Path)
		s.accepts = append(s.accepts, r.Header.Get("Accept"))
		tok := "ghs_installation_token_" + itoa(s.tokenN)
		exp := s.expiry
		s.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      tok,
			"expires_at": exp.Format(time.RFC3339),
		})
	}))
	return s, srv
}

func (s *mintStub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// fakeClock is a controllable time source for cache-expiry testing.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

func newTestMinter(srvURL string, clk *fakeClock) *Minter {
	return &Minter{
		client:  http.DefaultClient,
		apiBase: strings.TrimSuffix(srvURL, "/"),
		now:     clk.now,
		entries: map[string]*entry{},
	}
}

// TestMintAndCache asserts a mint hits the API once, returns the stub token with
// the right JWT auth/path/accept, and that a second call within TTL is served
// from cache WITHOUT re-hitting the stub.
func TestMintAndCache(t *testing.T) {
	stub, srv := newMintStub()
	defer srv.Close()
	t0 := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	stub.expiry = t0.Add(time.Hour)
	clk := &fakeClock{t: t0}
	m := newTestMinter(srv.URL, clk)

	cfg := Config{AppID: "123456", PrivateKeyPEM: testKeyPKCS1, InstallationID: "789"}

	tok1, exp1, err := m.Token(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok1 != "ghs_installation_token_1" {
		t.Errorf("token = %q, want minted token", tok1)
	}
	if !exp1.Equal(t0.Add(time.Hour)) {
		t.Errorf("expiry = %v, want %v", exp1, t0.Add(time.Hour))
	}
	if stub.count() != 1 {
		t.Fatalf("stub hits = %d after first mint, want 1", stub.count())
	}
	// Request shape.
	stub.mu.Lock()
	if !strings.HasPrefix(stub.auths[0], "Bearer ") {
		t.Errorf("auth = %q, want Bearer <jwt>", stub.auths[0])
	}
	if stub.paths[0] != "/app/installations/789/access_tokens" {
		t.Errorf("path = %q", stub.paths[0])
	}
	if stub.accepts[0] != "application/vnd.github+json" {
		t.Errorf("accept = %q", stub.accepts[0])
	}
	stub.mu.Unlock()

	// Advance a minute — still well within TTL: MUST be served from cache.
	clk.set(t0.Add(time.Minute))
	tok2, _, err := m.Token(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Token (cached): %v", err)
	}
	if tok2 != tok1 {
		t.Errorf("cached token = %q, want same as first %q", tok2, tok1)
	}
	if stub.count() != 1 {
		t.Fatalf("stub hits = %d after cached call, want 1 (no re-mint)", stub.count())
	}
}

// TestRefreshNearExpiry asserts that once the cached token is within the refresh
// window of expiry, the next Token() re-mints (a second stub hit, new token).
func TestRefreshNearExpiry(t *testing.T) {
	stub, srv := newMintStub()
	defer srv.Close()
	t0 := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	stub.expiry = t0.Add(time.Hour)
	clk := &fakeClock{t: t0}
	m := newTestMinter(srv.URL, clk)
	cfg := Config{AppID: "123456", PrivateKeyPEM: testKeyPKCS1, InstallationID: "789"}

	tok1, _, err := m.Token(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if stub.count() != 1 {
		t.Fatalf("hits = %d, want 1", stub.count())
	}

	// Jump to within refreshWindow of expiry (expiry - 4m < expiry - 5m? no:
	// 56m in is 4m before the 60m expiry, inside the 5m window) → must re-mint.
	clk.set(t0.Add(56 * time.Minute))
	stub.mu.Lock()
	stub.expiry = clk.now().Add(time.Hour) // fresh token gets a fresh hour
	stub.mu.Unlock()

	tok2, exp2, err := m.Token(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if stub.count() != 2 {
		t.Fatalf("hits = %d after near-expiry call, want 2 (re-mint)", stub.count())
	}
	if tok2 == tok1 {
		t.Errorf("re-mint returned the same token %q; want a fresh one", tok2)
	}
	if !exp2.After(t0.Add(time.Hour)) {
		t.Errorf("refreshed expiry %v should be later than original", exp2)
	}
}

// TestMintErrorNeverLeaksSecrets asserts a non-2xx mint returns an error that
// contains neither the private key nor any token material.
func TestMintErrorNeverLeaksSecrets(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	clk := &fakeClock{t: time.Now()}
	m := newTestMinter(srv.URL, clk)
	_, _, err := m.Token(context.Background(), Config{
		AppID: "1", PrivateKeyPEM: testKeyPKCS1, InstallationID: "2",
	})
	if err == nil {
		t.Fatal("want error on 401")
	}
	if strings.Contains(err.Error(), "PRIVATE KEY") || strings.Contains(err.Error(), "MIIEow") {
		t.Errorf("error text leaked key material: %q", err)
	}
}

// TestIncompleteConfigRejected asserts missing fields error out before any API call.
func TestIncompleteConfigRejected(t *testing.T) {
	m := New()
	for _, cfg := range []Config{
		{AppID: "", PrivateKeyPEM: testKeyPKCS1, InstallationID: "2"},
		{AppID: "1", PrivateKeyPEM: "", InstallationID: "2"},
		{AppID: "1", PrivateKeyPEM: testKeyPKCS1, InstallationID: ""},
	} {
		if _, _, err := m.Token(context.Background(), cfg); err == nil {
			t.Errorf("expected error for incomplete config %+v", cfg)
		}
	}
}
