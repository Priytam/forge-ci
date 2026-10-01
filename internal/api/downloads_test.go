package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestServeDownloadAllowlist(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOWNLOADS_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "install-runner.sh"), []byte("#!/bin/sh\necho hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	srv := &Server{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/downloads/{file}", srv.serveDownload)

	cases := []struct {
		name       string
		path       string
		wantStatus int
		wantBody   string
	}{
		{"allowlisted file present on disk", "/api/v1/downloads/install-runner.sh", http.StatusOK, "#!/bin/sh\necho hi\n"},
		{"allowlisted file missing on disk", "/api/v1/downloads/install-runner.ps1", http.StatusNotFound, ""},
		{"not on the allowlist at all", "/api/v1/downloads/etc-passwd", http.StatusNotFound, ""},
		{"path traversal attempt", "/api/v1/downloads/..%2f..%2fetc%2fpasswd", http.StatusNotFound, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, c.path, nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != c.wantStatus {
				t.Fatalf("status = %d, want %d (body: %s)", rec.Code, c.wantStatus, rec.Body.String())
			}
			if c.wantBody != "" && rec.Body.String() != c.wantBody {
				t.Fatalf("body = %q, want %q", rec.Body.String(), c.wantBody)
			}
		})
	}
}

func TestServeDownloadContentType(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOWNLOADS_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "forge-runner-linux-amd64"), []byte("binary-stand-in"), 0o644); err != nil {
		t.Fatal(err)
	}

	srv := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/downloads/forge-runner-linux-amd64", nil)
	req.SetPathValue("file", "forge-runner-linux-amd64")
	rec := httptest.NewRecorder()
	srv.serveDownload(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("Content-Type = %q, want application/octet-stream", ct)
	}
}
