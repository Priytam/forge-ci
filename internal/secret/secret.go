// Package secret provides envelope encryption for values stored at rest
// (CI/CD variables, VCS tokens, SSO client secrets).
//
// Ciphertext is tagged with a version prefix ("enc:v1:") so a value can be
// examined without a key: anything without the prefix is treated as
// pre-existing plaintext and returned as-is by Decrypt, which lets a
// previously unencrypted deployment migrate transparently. A Cipher with no
// key runs in passthrough mode (Encrypt is a no-op) so existing dev setups
// keep working; reading an encrypted value in that mode fails closed.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// Prefix marks an encrypted value. v1 = AES-256-GCM, nonce||ciphertext,
// base64-standard-encoded.
const Prefix = "enc:v1:"

// Cipher performs versioned AES-256-GCM encryption. The zero value (and any
// Cipher whose aead is nil) is a valid passthrough cipher.
type Cipher struct {
	aead cipher.AEAD // nil => passthrough mode (no key configured)
}

// FromEnv builds a Cipher from FORGE_SECRET_KEY (base64-encoded 32 bytes).
// When the variable is unset the returned Cipher runs in passthrough mode.
func FromEnv() (*Cipher, error) {
	b64 := strings.TrimSpace(os.Getenv("FORGE_SECRET_KEY"))
	if b64 == "" {
		return &Cipher{}, nil
	}
	key, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("FORGE_SECRET_KEY must be base64: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("FORGE_SECRET_KEY must decode to 32 bytes (got %d)", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Cipher{aead: aead}, nil
}

// HasKey reports whether a real key is configured (i.e. not passthrough).
func (c *Cipher) HasKey() bool { return c != nil && c.aead != nil }

// IsEncrypted reports whether value carries the versioned ciphertext prefix.
func IsEncrypted(value string) bool { return strings.HasPrefix(value, Prefix) }

// Encrypt returns the versioned ciphertext for plaintext. It is a no-op for:
//   - the empty string (empty secrets stay empty so "keep existing" upsert
//     semantics are preserved),
//   - values that are already encrypted (idempotent, so re-encryption passes
//     are safe),
//   - passthrough mode (no key configured).
func (c *Cipher) Encrypt(plaintext string) (string, error) {
	if plaintext == "" || IsEncrypted(plaintext) || !c.HasKey() {
		return plaintext, nil
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ct := c.aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return Prefix + base64.StdEncoding.EncodeToString(ct), nil
}

// Decrypt reverses Encrypt. Non-prefixed values are returned unchanged
// (pre-existing plaintext). Decrypting a prefixed value without a key, or with
// the wrong key, returns an error — never a silent empty value.
func (c *Cipher) Decrypt(value string) (string, error) {
	if !IsEncrypted(value) {
		return value, nil
	}
	if !c.HasKey() {
		return "", errors.New("value is encrypted but FORGE_SECRET_KEY is not configured")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(value, Prefix))
	if err != nil {
		return "", fmt.Errorf("decrypt: bad base64: %w", err)
	}
	ns := c.aead.NonceSize()
	if len(raw) < ns {
		return "", errors.New("decrypt: ciphertext too short")
	}
	pt, err := c.aead.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		return "", fmt.Errorf("decrypt: %w (wrong FORGE_SECRET_KEY?)", err)
	}
	return string(pt), nil
}
