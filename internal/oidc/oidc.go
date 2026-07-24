// Package oidc issues short-lived, RS256-signed OpenID Connect ID tokens, one
// per CI job, so a job can exchange it for cloud credentials with NO static
// cloud keys:
//
//   - AWS STS AssumeRoleWithWebIdentity, and
//   - GCP Workload Identity Federation.
//
// The cloud provider validates the token against Forge's published JWKS
// (fetched over the issuer's OIDC discovery document), so the whole trust chain
// is a public key — Forge never holds cloud credentials and the job never holds
// a long-lived secret.
//
// This package is pure crypto: it signs tokens and renders the discovery / JWKS
// documents. Key persistence (env or encrypted-in-DB) lives in internal/store,
// route wiring in internal/api. The RS256 signing mirrors the style already used
// by internal/githubapp (crypto/rsa PKCS1v15 over a SHA-256 digest, base64url
// JWT segments) rather than pulling in a JWT dependency.
package oidc

import (
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
	"math/big"
	"os"
	"strings"
	"time"
)

// TokenTTL is the validity window of a minted ID token. Short-lived by design:
// a job exchanges it for cloud credentials immediately at start, so 15 minutes
// is ample and bounds the blast radius if a token leaks.
const TokenTTL = 15 * time.Minute

// clockSkew backdates iat/nbf so a validator with a slightly fast clock still
// accepts a freshly minted token.
const clockSkew = 60 * time.Second

// DefaultAudience is used when OIDC_AUDIENCE is unset. AWS and GCP each expect a
// specific audience value; see docs/oidc.md for overriding it per cloud.
const DefaultAudience = "forge-ci"

// Signer holds the RSA keypair used to sign ID tokens and to publish the JWKS.
// It is immutable after construction and safe for concurrent use.
type Signer struct {
	key *rsa.PrivateKey
	kid string
	now func() time.Time
}

// Claims is the identity Forge asserts about one job. The standard registered
// claims (iss/aud/sub/iat/exp/nbf/jti) are filled by Mint; the caller supplies
// the job-identity fields.
type Claims struct {
	Repo        string
	Ref         string
	SHA         string
	PipelineID  int64
	JobID       int64
	Environment string
}

// NewSigner builds a Signer from an RSA private key, deriving a stable key id
// (kid) deterministically from the public key so the JWKS kid never changes for
// a given key across restarts.
func NewSigner(key *rsa.PrivateKey) *Signer {
	return &Signer{key: key, kid: KeyID(&key.PublicKey), now: time.Now}
}

// KID returns the key id published in the JWKS and stamped in each token header.
func (s *Signer) KID() string { return s.kid }

// Subject builds the token subject: a stable string encoding the job's identity
// that a cloud trust policy conditions on. Format:
//
//	repo:{repo}:ref:{ref}                       (no environment)
//	repo:{repo}:ref:{ref}:environment:{env}     (environment-targeting job)
//
// GitLab uses the same shape, so existing AWS/GCP condition snippets transfer.
func Subject(repo, ref, environment string) string {
	sub := "repo:" + repo + ":ref:" + ref
	if environment != "" {
		sub += ":environment:" + environment
	}
	return sub
}

// Mint issues a signed RS256 ID token for one job. issuer must be the public
// origin of the server (EXTERNAL_URL) and MUST match the issuer in the discovery
// document, or cloud validation fails. audience is the expected `aud`.
func (s *Signer) Mint(issuer, audience string, c Claims) (string, error) {
	if s == nil || s.key == nil {
		return "", errors.New("oidc: signer has no key")
	}
	now := s.now()
	jti, err := randomID()
	if err != nil {
		return "", err
	}
	header := map[string]any{"alg": "RS256", "typ": "JWT", "kid": s.kid}
	claims := map[string]any{
		"iss":         issuer,
		"aud":         audience,
		"sub":         Subject(c.Repo, c.Ref, c.Environment),
		"iat":         now.Add(-clockSkew).Unix(),
		"nbf":         now.Add(-clockSkew).Unix(),
		"exp":         now.Add(TokenTTL).Unix(),
		"jti":         jti,
		"repo":        c.Repo,
		"ref":         c.Ref,
		"sha":         c.SHA,
		"pipeline_id": c.PipelineID,
		"job_id":      c.JobID,
		"environment": c.Environment,
	}
	hb, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	cb, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signingInput := b64(hb) + "." + b64(cb)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// JWK is one key in a JWKS document (RSA public key, JSON serialization).
type JWK struct {
	Kty string `json:"kty"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	N   string `json:"n"`
	E   string `json:"e"`
}

// JWKS is the public key set a cloud provider fetches to verify tokens.
type JWKS struct {
	Keys []JWK `json:"keys"`
}

// JWKS renders the signer's public key as a one-key JWK set.
func (s *Signer) JWKS() JWKS {
	return JWKS{Keys: []JWK{PublicJWK(&s.key.PublicKey, s.kid)}}
}

// PublicJWK renders an RSA public key as a JWK with the given kid.
func PublicJWK(pub *rsa.PublicKey, kid string) JWK {
	return JWK{
		Kty: "RSA",
		Use: "sig",
		Alg: "RS256",
		Kid: kid,
		N:   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		E:   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}
}

// Discovery renders the minimal OpenID Provider metadata AWS and GCP need to
// locate the JWKS and understand the token. issuer MUST equal the `iss` claim.
func Discovery(issuer string) map[string]any {
	return map[string]any{
		"issuer":                                issuer,
		"jwks_uri":                              issuer + "/.well-known/jwks.json",
		"response_types_supported":              []string{"id_token"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"claims_supported": []string{
			"iss", "aud", "sub", "iat", "nbf", "exp", "jti",
			"repo", "ref", "sha", "pipeline_id", "job_id", "environment",
		},
		"scopes_supported": []string{"openid"},
	}
}

// KeyID derives a deterministic, stable key id from an RSA public key: the
// base64url of the SHA-256 of its PKIX DER encoding, truncated. Deterministic so
// the same key always yields the same kid (JWKS stability across restarts).
func KeyID(pub *rsa.PublicKey) string {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		// Fall back to hashing the modulus; MarshalPKIXPublicKey never fails for
		// a valid RSA key in practice.
		der = pub.N.Bytes()
	}
	sum := sha256.Sum256(der)
	return base64.RawURLEncoding.EncodeToString(sum[:])[:16]
}

// GenerateKey creates a fresh RSA-2048 signing key.
func GenerateKey() (*rsa.PrivateKey, error) {
	return rsa.GenerateKey(rand.Reader, 2048)
}

// MarshalPrivateKeyPEM encodes a private key as PKCS#8 PEM (for persistence).
func MarshalPrivateKeyPEM(key *rsa.PrivateKey) (string, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), nil
}

// ParsePrivateKeyPEM accepts an RSA private key in PKCS#1 ("RSA PRIVATE KEY") or
// PKCS#8 ("PRIVATE KEY") PEM. Used for the OIDC_PRIVATE_KEY env override and for
// reading the persisted key back. The error text never contains the key.
func ParsePrivateKeyPEM(pemStr string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(strings.TrimSpace(pemStr)))
	if block == nil {
		return nil, errors.New("oidc: private key is not valid PEM")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k8, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("oidc: private key must be an RSA key in PKCS#1 or PKCS#8 PEM")
	}
	rsaKey, ok := k8.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("oidc: private key is not an RSA key")
	}
	return rsaKey, nil
}

// Audience returns the configured token audience (OIDC_AUDIENCE) or the default.
func Audience() string {
	if v := strings.TrimSpace(os.Getenv("OIDC_AUDIENCE")); v != "" {
		return v
	}
	return DefaultAudience
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func randomID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("oidc: random jti: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
