package secret

import (
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
)

func keyedCipher(t *testing.T) *Cipher {
	t.Helper()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FORGE_SECRET_KEY", base64.StdEncoding.EncodeToString(raw))
	c, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if !c.HasKey() {
		t.Fatal("expected keyed cipher")
	}
	return c
}

func TestRoundTrip(t *testing.T) {
	c := keyedCipher(t)
	for _, pt := range []string{"s3cr3t-value", "with spaces and \n newlines", "unicode ✓"} {
		enc, err := c.Encrypt(pt)
		if err != nil {
			t.Fatal(err)
		}
		if !IsEncrypted(enc) {
			t.Fatalf("ciphertext missing prefix: %q", enc)
		}
		if strings.Contains(enc, pt) {
			t.Fatalf("plaintext leaked into ciphertext: %q", enc)
		}
		got, err := c.Decrypt(enc)
		if err != nil {
			t.Fatal(err)
		}
		if got != pt {
			t.Fatalf("round trip mismatch: got %q want %q", got, pt)
		}
	}
}

func TestEmptyIsPassthrough(t *testing.T) {
	c := keyedCipher(t)
	enc, err := c.Encrypt("")
	if err != nil || enc != "" {
		t.Fatalf("empty should stay empty, got %q err %v", enc, err)
	}
}

func TestEncryptIsIdempotent(t *testing.T) {
	c := keyedCipher(t)
	enc, _ := c.Encrypt("hello")
	again, err := c.Encrypt(enc)
	if err != nil || again != enc {
		t.Fatalf("re-encrypting an enc value must be a no-op: %q -> %q", enc, again)
	}
}

func TestPlaintextPassthroughOnDecrypt(t *testing.T) {
	c := keyedCipher(t)
	got, err := c.Decrypt("legacy-plaintext")
	if err != nil || got != "legacy-plaintext" {
		t.Fatalf("non-prefixed value must pass through: %q err %v", got, err)
	}
}

func TestPassthroughModeNoKey(t *testing.T) {
	t.Setenv("FORGE_SECRET_KEY", "")
	c, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if c.HasKey() {
		t.Fatal("expected passthrough cipher")
	}
	enc, _ := c.Encrypt("value")
	if enc != "value" {
		t.Fatalf("passthrough Encrypt must be a no-op, got %q", enc)
	}
	// Reading a plaintext value works.
	if got, _ := c.Decrypt("value"); got != "value" {
		t.Fatal("plaintext read should work in passthrough")
	}
	// Reading an encrypted value without a key must FAIL closed.
	if _, err := c.Decrypt(Prefix + "abc"); err == nil {
		t.Fatal("decrypting enc value without key must error, not return empty")
	}
}

func TestWrongKeyFailsClosed(t *testing.T) {
	c1 := keyedCipher(t)
	enc, _ := c1.Encrypt("value")
	// Fresh key.
	raw := make([]byte, 32)
	rand.Read(raw)
	t.Setenv("FORGE_SECRET_KEY", base64.StdEncoding.EncodeToString(raw))
	c2, _ := FromEnv()
	if _, err := c2.Decrypt(enc); err == nil {
		t.Fatal("decrypting with wrong key must error")
	}
}

func TestBadKeyRejected(t *testing.T) {
	t.Setenv("FORGE_SECRET_KEY", base64.StdEncoding.EncodeToString([]byte("too-short")))
	if _, err := FromEnv(); err == nil {
		t.Fatal("expected error for non-32-byte key")
	}
}
