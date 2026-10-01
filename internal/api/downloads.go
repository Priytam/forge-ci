package api

import (
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// downloadableFiles is the exact allowlist served under /api/v1/downloads/ —
// deliberately not a general directory listing, so this can never become a
// path-traversal or arbitrary-file-read bug regardless of what else ends up
// in downloadsDir.
var downloadableFiles = map[string]string{
	"forge-runner-darwin-arm64":      "application/octet-stream",
	"forge-runner-darwin-amd64":      "application/octet-stream",
	"forge-runner-linux-amd64":       "application/octet-stream",
	"forge-runner-windows-amd64.exe": "application/octet-stream",
	"install-runner.sh":              "text/x-shellscript; charset=utf-8",
	"install-runner.ps1":             "text/plain; charset=utf-8",
}

// downloadsDir is where deploy/Dockerfile.server stages the prebuilt runner
// binaries and install scripts. Overridable for local dev (`make server`
// does not run from inside that image).
func downloadsDir() string {
	if v := os.Getenv("DOWNLOADS_DIR"); v != "" {
		return v
	}
	return "/usr/local/share/forge-downloads"
}

// registerDownloadRoutes serves the self-service "register this machine as a
// runner" assets: prebuilt forge-runner binaries for the common platforms
// plus a one-line install script per OS. No login required — these are
// public binaries of an open-source tool, same trust level as the runner
// protocol itself (see authExempt), and requiring a session just to grab a
// binary would defeat the point of a copy-paste install line.
func (s *Server) registerDownloadRoutes() {
	s.mux.HandleFunc("GET /api/v1/downloads/{file}", s.serveDownload)
}

func (s *Server) serveDownload(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	contentType, ok := downloadableFiles[name]
	if !ok {
		writeErr(w, http.StatusNotFound, "no such download")
		return
	}
	path := filepath.Join(downloadsDir(), name)
	f, err := os.Open(path)
	if err != nil {
		writeErr(w, http.StatusNotFound, "download not available on this server")
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", contentType)
	http.ServeContent(w, r, name, time.Time{}, f)
}
