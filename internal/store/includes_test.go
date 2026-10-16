package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/priytamjeepandey/forge-ci/internal/compiler"
	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

func TestCompareVersions(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"1.10.0", "1.9.2", 1}, {"1.0.0", "1.0.0-rc1", 1}, {"v2", "1.9", 1}, {"1.2", "1.2.0", 0}, {"0.9", "1.0", -1},
	} {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compare(%s,%s) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestMayIncludeFrom(t *testing.T) {
	if !mayIncludeFrom("acme/api", "acme/ci-templates") || !mayIncludeFrom("x", "x") {
		t.Error("same owner / same repo must be allowed")
	}
	if mayIncludeFrom("acme/api", "globex/secrets") || mayIncludeFrom("api", "ci") {
		t.Error("another owner must be refused by default")
	}
	t.Setenv("FORGE_INCLUDE_SHARED_PROJECTS", "platform/*, globex/public-ci")
	if !mayIncludeFrom("acme/api", "platform/ci") || !mayIncludeFrom("acme/api", "globex/public-ci") || mayIncludeFrom("acme/api", "globex/secrets") {
		t.Error("shared allowlist not applied")
	}
}

func TestRemoteIncludeGuards(t *testing.T) {
	ctx := context.Background()
	body := "jobs:\n  lint:\n    stage: test\n    script: [make lint]\n"
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
	defer srv.Close()
	tr := remoteClient.Transport.(*http.Transport)
	saved := tr.TLSClientConfig
	tr.TLSClientConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig
	defer func() { tr.TLSClientConfig = saved }()

	if _, err := fetchRemoteInclude(ctx, "http://example.com/x.yml", ""); err == nil || !strings.Contains(err.Error(), "https") {
		t.Errorf("plain http: %v", err)
	}
	if _, err := fetchRemoteInclude(ctx, srv.URL+"/ci.yml", ""); err == nil || !strings.Contains(err.Error(), "internal address") {
		t.Errorf("loopback must be blocked: %v", err)
	}
	t.Setenv("FORGE_INCLUDE_REMOTE_ALLOW_PRIVATE", "true")
	got, err := fetchRemoteInclude(ctx, srv.URL+"/ci.yml", "")
	if err != nil || got != body {
		t.Fatalf("fetch = %q %v", got, err)
	}
	sum := sha256.Sum256([]byte(body))
	if _, err := fetchRemoteInclude(ctx, srv.URL+"/ci.yml", "sha256:"+hex.EncodeToString(sum[:])); err != nil {
		t.Errorf("matching integrity: %v", err)
	}
	if _, err := fetchRemoteInclude(ctx, srv.URL+"/ci.yml", "sha256:00"); err == nil || !strings.Contains(err.Error(), "integrity mismatch") {
		t.Errorf("wrong integrity: %v", err)
	}
	t.Setenv("FORGE_INCLUDE_REMOTE_HOSTS", "*.example.com")
	if _, err := fetchRemoteInclude(ctx, srv.URL+"/ci.yml", ""); err == nil || !strings.Contains(err.Error(), "FORGE_INCLUDE_REMOTE_HOSTS") {
		t.Errorf("host allowlist: %v", err)
	}
}

func TestTemplateCatalog(t *testing.T) {
	st := newRegistryTestStore(t)
	ctx := context.Background()
	tmpl := "spec:\n  inputs:\n    env: {default: staging}\n---\njobs:\n  deploy:\n    stage: deploy\n    script: [\"deploy $[[ inputs.env ]]\"]\n"
	for _, v := range []string{"1.9.0", "1.10.0", "1.2.0"} {
		if err := st.PublishCatalogTemplate(ctx, proto.CatalogTemplate{Name: "deploy/k8s", Version: v, YAML: tmpl}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.PublishCatalogTemplate(ctx, proto.CatalogTemplate{Name: "deploy/k8s", Version: "1.2.0", YAML: tmpl}); !errors.Is(err, ErrVersionExists) {
		t.Errorf("republish = %v, want ErrVersionExists", err)
	}
	if err := st.PublishCatalogTemplate(ctx, proto.CatalogTemplate{Name: "Bad Name", Version: "1", YAML: tmpl}); err == nil {
		t.Error("bad name accepted")
	}
	latest, err := st.GetCatalogTemplate(ctx, "deploy/k8s", "")
	if err != nil || latest.Version != "1.10.0" || len(latest.Inputs) != 1 || latest.Inputs[0].Default != "staging" {
		t.Errorf("latest = %+v %v", latest, err)
	}
	yml := "stages: [deploy]\ninclude:\n  - template: deploy/k8s@1.2.0\n    inputs: {env: prod}\n"
	jobs, err := compiler.CompileWith(yml, "main", "api", st.CompileOpts(ctx, "acme/api"))
	if err != nil || len(jobs) != 1 || jobs[0].Script != "deploy prod" {
		t.Fatalf("compile via catalog = %+v %v", jobs, err)
	}
	if err := st.DeleteCatalogTemplate(ctx, "deploy/k8s", "1.2.0"); err != nil {
		t.Fatal(err)
	}
	if _, err := compiler.CompileWith(yml, "main", "api", st.CompileOpts(ctx, "acme/api")); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("deleted version: %v", err)
	}
}

// TestProjectInclude reads a template file from another connected repo on a
// plain git host (file:// here), at a tag.
func TestProjectInclude(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	st := newRegistryTestStore(t)
	ctx := context.Background()
	src := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = src
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "uploadpack.allowFilter", "true")
	run("config", "uploadpack.allowAnySHA1InWant", "true")
	_ = os.MkdirAll(filepath.Join(src, "ci"), 0o755)
	_ = os.WriteFile(filepath.Join(src, "ci", "lint.yml"), []byte("jobs:\n  lint:\n    stage: test\n    script: [make lint]\n"), 0o644)
	run("add", ".")
	run("commit", "-q", "-m", "templates")
	run("tag", "v1")
	if err := st.RegisterRepo(ctx, proto.RepoRegistration{Repo: "acme/ci-templates", Provider: "git", CloneURL: "file://" + src}); err != nil {
		t.Fatal(err)
	}
	yml := "stages: [test]\ninclude:\n  - project: acme/ci-templates\n    ref: v1\n    file: ci/lint.yml\n"
	jobs, err := compiler.CompileWith(yml, "main", "api", st.CompileOpts(ctx, "acme/api"))
	if err != nil || len(jobs) != 1 || jobs[0].Name != "lint" {
		t.Fatalf("project include = %+v %v", jobs, err)
	}
	if _, err := compiler.CompileWith(yml, "main", "api", st.CompileOpts(ctx, "globex/api")); err == nil || !strings.Contains(err.Error(), "may not include") {
		t.Errorf("cross-owner include: %v", err)
	}
	noRef := strings.Replace(yml, "    ref: v1\n", "", 1)
	if _, err := compiler.CompileWith(noRef, "main", "api", st.CompileOpts(ctx, "acme/api")); err == nil || !strings.Contains(err.Error(), "needs ref") {
		t.Errorf("missing ref: %v", err)
	}
}
