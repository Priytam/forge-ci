package api

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestWebhookLargePayloadKeepsSignature: a correctly signed 3 MB payload
// passes the HMAC check (it used to be truncated at 1 MiB, so the signature
// never matched), and a body over WEBHOOK_MAX_BYTES gets a clear 413.
func TestWebhookLargePayloadKeepsSignature(t *testing.T) {
	t.Setenv("WEBHOOK_SECRET", "s3cret")
	t.Setenv("WEBHOOK_MAX_BYTES", "4194304") // 4 MiB for the test
	st, _ := newCFRStore(t, "")
	srv := New(st, nil, nil)

	send := func(body []byte, sign bool) int {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(body))
		req.Header.Set("X-GitHub-Event", "ping")
		if sign {
			mac := hmac.New(sha256.New, []byte("s3cret"))
			mac.Write(body)
			req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec.Code
	}

	big := []byte(`{"zen":"` + strings.Repeat("x", 3<<20) + `"}`)
	if code := send(big, true); code != http.StatusAccepted {
		t.Errorf("signed 3 MB payload: status %d, want 202 (signature must verify over the full body)", code)
	}
	if code := send(big, false); code != http.StatusUnauthorized {
		t.Errorf("unsigned payload: status %d, want 401", code)
	}
	huge := []byte(`{"zen":"` + strings.Repeat("x", 5<<20) + `"}`)
	if code := send(huge, true); code != http.StatusRequestEntityTooLarge {
		t.Errorf("payload over the limit: status %d, want 413", code)
	}
}
