// Package githubapp mints short-lived GitHub App installation access tokens as
// an alternative to long-lived static PATs.
//
// A GitHub App is identified by an app id and an RSA private key (PEM). Once
// installed on an org/repo it has an installation id. To act as that
// installation Forge:
//
//  1. signs a short-lived JWT (RS256, iss=app id, iat/exp <= 10 min) with the
//     private key,
//  2. POSTs it to /app/installations/{id}/access_tokens, receiving a ~1h
//     installation access token,
//  3. uses that token exactly like a PAT (clone via x-access-token, commit
//     status via Bearer).
//
// Tokens auto-expire, so this package mints them on demand and caches each one
// in memory (keyed by app+installation) until it is near expiry, then re-mints.
// The GitHub API base is overridable via GITHUB_API_BASE (matching
// internal/vcs) so tests can point it at a stub. The private key and the minted
// tokens are secrets: they are never logged and never appear in returned errors.
package githubapp

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// refreshWindow: a cached token is re-minted once it is within this of expiry,
// so a token handed out is always comfortably valid for the caller's use.
const refreshWindow = 5 * time.Minute

// jwtTTL is the app JWT's validity. GitHub caps it at 10 minutes; 9 leaves room
// for the iat clock-skew backdate below.
const jwtTTL = 9 * time.Minute

// Config identifies one GitHub App installation to mint a token for.
type Config struct {
	AppID          string
	PrivateKeyPEM  string // sensitive — never logged
	InstallationID string
}

type entry struct {
	mu        sync.Mutex // serializes mint/refresh for this installation
	token     string
	expiresAt time.Time
}

// Minter mints and caches GitHub App installation access tokens. It is safe for
// concurrent use by multiple goroutines.
type Minter struct {
	client  *http.Client
	apiBase string
	now     func() time.Time

	mu      sync.Mutex        // guards entries
	entries map[string]*entry // keyed by appID + "/" + installationID
}

// New builds a Minter from the environment. GITHUB_API_BASE overrides the public
// API host (used for tests against a stub; leave unset in production).
func New() *Minter {
	return &Minter{
		client:  &http.Client{Timeout: 10 * time.Second},
		apiBase: apiBase(),
		now:     time.Now,
		entries: map[string]*entry{},
	}
}

func apiBase() string {
	if v := strings.TrimSpace(os.Getenv("GITHUB_API_BASE")); v != "" {
		return strings.TrimSuffix(v, "/")
	}
	return "https://api.github.com"
}

// Token returns a valid installation access token for cfg, minting a fresh one
// (and caching it) when there is no cached token or the cached one is within the
// refresh window of expiry. A second call within the token's TTL returns the
// cached value without hitting the GitHub API. Errors never contain the private
// key or a token.
func (m *Minter) Token(ctx context.Context, cfg Config) (string, time.Time, error) {
	if cfg.AppID == "" || cfg.InstallationID == "" || cfg.PrivateKeyPEM == "" {
		return "", time.Time{}, errors.New("githubapp: incomplete config (need app_id, installation_id and private_key)")
	}
	key := cfg.AppID + "/" + cfg.InstallationID

	m.mu.Lock()
	e, ok := m.entries[key]
	if !ok {
		e = &entry{}
		m.entries[key] = e
	}
	m.mu.Unlock()

	// Serialize per installation so concurrent callers don't stampede the API;
	// the network call is held under the per-entry lock, not the map lock.
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.token != "" && m.now().Before(e.expiresAt.Add(-refreshWindow)) {
		return e.token, e.expiresAt, nil
	}
	tok, exp, err := m.mint(ctx, cfg)
	if err != nil {
		return "", time.Time{}, err
	}
	e.token, e.expiresAt = tok, exp
	return tok, exp, nil
}

// mint performs the JWT sign + installation-token exchange against the GitHub
// API. It never logs and never returns the key or token in error text.
func (m *Minter) mint(ctx context.Context, cfg Config) (string, time.Time, error) {
	pk, err := parseRSAPrivateKey(cfg.PrivateKeyPEM)
	if err != nil {
		return "", time.Time{}, err
	}
	jwt, err := signJWT(cfg.AppID, pk, m.now())
	if err != nil {
		return "", time.Time{}, fmt.Errorf("githubapp: sign jwt: %w", err)
	}
	url := fmt.Sprintf("%s/app/installations/%s/access_tokens", m.apiBase, cfg.InstallationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := m.client.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("githubapp: mint request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Status only — the response body can echo request material; never the key/token.
		return "", time.Time{}, fmt.Errorf("githubapp: mint installation token failed: HTTP %d", resp.StatusCode)
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", time.Time{}, fmt.Errorf("githubapp: decode mint response: %w", err)
	}
	if out.Token == "" {
		return "", time.Time{}, errors.New("githubapp: mint response contained no token")
	}
	if out.ExpiresAt.IsZero() {
		// Defensive: GitHub always sends expires_at, but never cache "forever".
		out.ExpiresAt = m.now().Add(time.Hour)
	}
	return out.Token, out.ExpiresAt, nil
}

// signJWT builds and RS256-signs the app JWT. iss is the app id, iat is
// backdated 60s to tolerate clock skew, exp is now+jwtTTL (< GitHub's 10 min).
func signJWT(appID string, key *rsa.PrivateKey, now time.Time) (string, error) {
	header := b64(`{"alg":"RS256","typ":"JWT"}`)
	claims, err := json.Marshal(map[string]any{
		"iss": appID,
		"iat": now.Add(-60 * time.Second).Unix(),
		"exp": now.Add(jwtTTL).Unix(),
	})
	if err != nil {
		return "", err
	}
	signingInput := header + "." + b64(string(claims))
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func b64(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

// parseRSAPrivateKey accepts a GitHub App private key in PKCS#1 ("RSA PRIVATE
// KEY") or PKCS#8 ("PRIVATE KEY") PEM. The error text never contains the key.
func parseRSAPrivateKey(pemStr string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, errors.New("githubapp: private key is not valid PEM")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k8, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("githubapp: private key must be an RSA key in PKCS#1 or PKCS#8 PEM")
	}
	rsaKey, ok := k8.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("githubapp: private key is not an RSA key")
	}
	return rsaKey, nil
}
