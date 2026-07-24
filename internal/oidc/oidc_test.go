package oidc

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"strings"
	"testing"
	"time"
)

// TestMintVerifiesAgainstPublishedJWK is the crux: mint a token with the signer,
// then reconstruct the RSA public key SOLELY from the published JWKS (as AWS/GCP
// would) and cryptographically verify the RS256 signature, then assert the
// claims. This proves the token really validates against the published key.
func TestMintVerifiesAgainstPublishedJWK(t *testing.T) {
	key, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	signer := NewSigner(key)

	const issuer = "https://forge.example.com"
	const audience = "sts.amazonaws.com"
	claims := Claims{
		Repo:        "acme/widgets",
		Ref:         "refs/heads/main",
		SHA:         "deadbeefcafe",
		PipelineID:  42,
		JobID:       1001,
		Environment: "production",
	}
	tok, err := signer.Mint(issuer, audience, claims)
	if err != nil {
		t.Fatal(err)
	}

	// --- reconstruct the public key from the JWKS only ---
	jwks := signer.JWKS()
	if len(jwks.Keys) != 1 {
		t.Fatalf("want 1 JWK, got %d", len(jwks.Keys))
	}
	jwk := jwks.Keys[0]
	if jwk.Kty != "RSA" || jwk.Alg != "RS256" || jwk.Use != "sig" {
		t.Fatalf("unexpected JWK metadata: %+v", jwk)
	}
	if jwk.Kid == "" || jwk.Kid != signer.KID() {
		t.Fatalf("kid mismatch: jwk=%q signer=%q", jwk.Kid, signer.KID())
	}
	pub := pubKeyFromJWK(t, jwk)

	// --- verify the RS256 signature with that reconstructed key ---
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("token is not a 3-part JWT: %d parts", len(parts))
	}
	signingInput := parts[0] + "." + parts[1]
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	digest := sha256.Sum256([]byte(signingInput))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig); err != nil {
		t.Fatalf("SIGNATURE DID NOT VERIFY against published JWK: %v", err)
	}

	// --- header kid matches the JWKS kid ---
	var hdr map[string]any
	decodeSeg(t, parts[0], &hdr)
	if hdr["alg"] != "RS256" || hdr["kid"] != jwk.Kid {
		t.Fatalf("header mismatch: %+v", hdr)
	}

	// --- assert the claims ---
	var c map[string]any
	decodeSeg(t, parts[1], &c)
	assertStr(t, c, "iss", issuer)
	assertStr(t, c, "aud", audience)
	assertStr(t, c, "sub", "repo:acme/widgets:ref:refs/heads/main:environment:production")
	assertStr(t, c, "repo", "acme/widgets")
	assertStr(t, c, "ref", "refs/heads/main")
	assertStr(t, c, "sha", "deadbeefcafe")
	assertStr(t, c, "environment", "production")
	if c["pipeline_id"].(float64) != 42 {
		t.Fatalf("pipeline_id = %v", c["pipeline_id"])
	}
	if c["job_id"].(float64) != 1001 {
		t.Fatalf("job_id = %v", c["job_id"])
	}
	if c["jti"].(string) == "" {
		t.Fatal("jti is empty")
	}

	// exp is ~15 min out; iat/nbf are in the past (skew backdate).
	iat := int64(c["iat"].(float64))
	exp := int64(c["exp"].(float64))
	nbf := int64(c["nbf"].(float64))
	ttl := time.Duration(exp-iat) * time.Second
	// iat is backdated by clockSkew, so exp-iat = TokenTTL + clockSkew.
	if ttl < TokenTTL || ttl > TokenTTL+2*clockSkew {
		t.Fatalf("token TTL out of range: %s (want ~%s)", ttl, TokenTTL)
	}
	now := time.Now().Unix()
	if nbf > now {
		t.Fatalf("nbf %d is in the future (now %d)", nbf, now)
	}
	if exp <= now {
		t.Fatalf("token already expired: exp %d <= now %d", exp, now)
	}
}

// TestSubjectFormat covers the environment-less subject shape.
func TestSubjectFormat(t *testing.T) {
	if got := Subject("acme/app", "refs/heads/dev", ""); got != "repo:acme/app:ref:refs/heads/dev" {
		t.Fatalf("no-env subject = %q", got)
	}
	if got := Subject("acme/app", "refs/heads/main", "staging"); got != "repo:acme/app:ref:refs/heads/main:environment:staging" {
		t.Fatalf("env subject = %q", got)
	}
}

// TestKeyIDStable ensures a round-tripped (PEM-persisted) key keeps the same kid,
// so restarts don't invalidate the published JWKS.
func TestKeyIDStable(t *testing.T) {
	key, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	pem, err := MarshalPrivateKeyPEM(key)
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParsePrivateKeyPEM(pem)
	if err != nil {
		t.Fatal(err)
	}
	if a, b := KeyID(&key.PublicKey), KeyID(&back.PublicKey); a != b {
		t.Fatalf("kid changed across PEM round-trip: %q vs %q", a, b)
	}
}

func pubKeyFromJWK(t *testing.T, jwk JWK) *rsa.PublicKey {
	t.Helper()
	nb, err := base64.RawURLEncoding.DecodeString(jwk.N)
	if err != nil {
		t.Fatalf("decode n: %v", err)
	}
	eb, err := base64.RawURLEncoding.DecodeString(jwk.E)
	if err != nil {
		t.Fatalf("decode e: %v", err)
	}
	return &rsa.PublicKey{
		N: new(big.Int).SetBytes(nb),
		E: int(new(big.Int).SetBytes(eb).Int64()),
	}
}

func decodeSeg(t *testing.T, seg string, v any) {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		t.Fatalf("decode segment: %v", err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("unmarshal segment: %v", err)
	}
}

func assertStr(t *testing.T, m map[string]any, key, want string) {
	t.Helper()
	if got, _ := m[key].(string); got != want {
		t.Fatalf("claim %q = %q, want %q", key, got, want)
	}
}
