package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/executor"
	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

// TestTokenFileRefreshes: the job's OIDC token lands in a file the script can
// read, FORGE_OIDC_TOKEN_FILE points at it, and the runner rewrites it with a
// freshly minted token until the job ends — so long builds keep valid
// credentials. Stopping removes the directory.
func TestTokenFileRefreshes(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/runner/jobs/7/oidc-token" || r.Header.Get("Authorization") != "Bearer rt" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		i := n.Add(1)
		_ = json.NewEncoder(w).Encode(proto.OIDCTokenResponse{Token: "fresh-" + string(rune('0'+i)), ExpiresAt: time.Now().Add(15 * time.Minute)})
	}))
	defer srv.Close()

	old := tokenRefreshEvery
	tokenRefreshEvery = 20 * time.Millisecond
	defer func() { tokenRefreshEvery = old }()

	c := &client{base: srv.URL, http: srv.Client(), token: "rt"}
	logs := newLogStreamer(c, 7, nil)
	defer logs.close()
	ex, err := executor.New("shell")
	if err != nil {
		t.Fatal(err)
	}
	job := &proto.RunnerJob{ID: 7, Env: map[string]string{"FORGE_OIDC_TOKEN": "initial"}}
	stop := startTokenRefresh(context.Background(), c, logs, ex, job)

	path := job.Env["FORGE_OIDC_TOKEN_FILE"]
	if path == "" || job.Env["AWS_WEB_IDENTITY_TOKEN_FILE"] != path {
		t.Fatalf("token file env = %q / %q", path, job.Env["AWS_WEB_IDENTITY_TOKEN_FILE"])
	}
	if b, _ := os.ReadFile(path); string(b) != "initial" {
		t.Fatalf("initial token file = %q", b)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if b, _ := os.ReadFile(path); strings.HasPrefix(string(b), "fresh-") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if b, _ := os.ReadFile(path); !strings.HasPrefix(string(b), "fresh-") {
		t.Fatalf("token file was not refreshed: %q", b)
	}
	stop()
	if _, err := os.Stat(job.RunDir); !os.IsNotExist(err) {
		t.Errorf("run dir not removed after the job: %v", err)
	}
}

func TestTokenRefreshNoTokenIsNoop(t *testing.T) {
	ex, _ := executor.New("shell")
	job := &proto.RunnerJob{ID: 1, Env: map[string]string{}}
	stop := startTokenRefresh(context.Background(), &client{}, newLogStreamer(&client{}, 1, nil), ex, job)
	stop()
	if _, ok := job.Env["FORGE_OIDC_TOKEN_FILE"]; ok || job.RunDir != "" {
		t.Error("no token should mean no token file")
	}
}

func TestRunPathPerExecutor(t *testing.T) {
	job := &proto.RunnerJob{RunDir: "/tmp/forge-run-x"}
	shell, _ := executor.New("shell")
	docker, _ := executor.New("docker")
	if p := executor.RunPath(shell, job, "oidc-token"); p != "/tmp/forge-run-x/oidc-token" {
		t.Errorf("shell path = %s", p)
	}
	if p := executor.RunPath(docker, job, "oidc-token"); p != "/var/run/forge/oidc-token" {
		t.Errorf("docker path = %s", p)
	}
}
