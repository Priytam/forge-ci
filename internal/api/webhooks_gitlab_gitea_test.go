package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
)

func postHook(srv *Server, path string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func TestGitLabWebhook(t *testing.T) {
	t.Setenv("WEBHOOK_SECRET", "s3cret")
	st, db := newCFRStore(t, "")
	ctx := context.Background()
	const repo = "acme/platform/cos"
	if _, err := st.SetRepoConfig(ctx, repo, plainYAML, "t", "init"); err != nil {
		t.Fatal(err)
	}
	srv := New(st, nil, nil)
	push := []byte(`{"object_kind":"push","ref":"refs/heads/main","after":"aaaa1111","checkout_sha":"aaaa1111",
	  "user_username":"harish","project":{"path_with_namespace":"acme/platform/cos"},
	  "commits":[{"id":"aaaa1111","message":"Fix totals","author":{"name":"Harish"}}]}`)
	hdr := map[string]string{"X-Gitlab-Event": "Push Hook", "X-Gitlab-Token": "s3cret", "X-Gitlab-Event-UUID": "u1"}

	if rec := postHook(srv, "/api/v1/webhooks/gitlab", push, map[string]string{"X-Gitlab-Event": "Push Hook", "X-Gitlab-Token": "nope"}); rec.Code != 401 {
		t.Fatalf("bad token = %d", rec.Code)
	}
	if rec := postHook(srv, "/api/v1/webhooks/gitlab", push, hdr); rec.Code != 201 {
		t.Fatalf("push = %d %s", rec.Code, rec.Body.String())
	}
	var triggeredBy, author, message, source string
	_ = db.QueryRow(ctx, `SELECT triggered_by, commit_author, commit_message, source FROM pipelines WHERE repo=$1`, repo).
		Scan(&triggeredBy, &author, &message, &source)
	if triggeredBy != "harish" || author != "Harish" || message != "Fix totals" || source != "push" {
		t.Errorf("pipeline = %s/%s/%s/%s", triggeredBy, author, message, source)
	}
	if rec := postHook(srv, "/api/v1/webhooks/gitlab", push, hdr); rec.Code != 200 {
		t.Errorf("redelivery = %d, want 200 duplicate", rec.Code)
	}
	del := []byte(`{"object_kind":"push","ref":"refs/heads/old","after":"0000000000000000000000000000000000000000","project":{"path_with_namespace":"acme/platform/cos"}}`)
	if rec := postHook(srv, "/api/v1/webhooks/gitlab", del, map[string]string{"X-Gitlab-Event": "Push Hook", "X-Gitlab-Token": "s3cret"}); rec.Code != 202 {
		t.Errorf("branch delete = %d, want 202", rec.Code)
	}
	mr := []byte(`{"object_kind":"merge_request","user":{"username":"ashwin"},"project":{"path_with_namespace":"acme/platform/cos"},
	  "object_attributes":{"iid":7,"title":"Add promo","source_branch":"promo","target_branch":"main","action":"open","last_commit":{"id":"bbbb2222"}}}`)
	if rec := postHook(srv, "/api/v1/webhooks/gitlab", mr, map[string]string{"X-Gitlab-Event": "Merge Request Hook", "X-Gitlab-Token": "s3cret"}); rec.Code != 201 {
		t.Fatalf("MR = %d %s", rec.Code, rec.Body.String())
	}
	var mrIID, ref string
	_ = db.QueryRow(ctx, `SELECT mr_iid, ref FROM pipelines WHERE source='merge_request'`).Scan(&mrIID, &ref)
	if mrIID != "7" || ref != "promo" {
		t.Errorf("MR pipeline = iid %s ref %s", mrIID, ref)
	}
	var mrID, mrPath string
	_ = db.QueryRow(ctx, `SELECT j.env->>'CI_MERGE_REQUEST_ID', j.env->>'CI_MERGE_REQUEST_PROJECT_PATH'
	  FROM jobs j JOIN pipelines p ON p.id = j.pipeline_id WHERE p.source='merge_request' LIMIT 1`).Scan(&mrID, &mrPath)
	if mrID != "7" || mrPath != repo {
		t.Errorf("MR job vars = id %q path %q", mrID, mrPath)
	}
	edit := []byte(`{"object_kind":"merge_request","project":{"path_with_namespace":"acme/platform/cos"},
	  "object_attributes":{"iid":7,"action":"update","last_commit":{"id":"bbbb2222"}}}`)
	if rec := postHook(srv, "/api/v1/webhooks/gitlab", edit, map[string]string{"X-Gitlab-Event": "Merge Request Hook", "X-Gitlab-Token": "s3cret"}); rec.Code != 202 {
		t.Errorf("title-only update = %d, want 202 (no new commits)", rec.Code)
	}
}

func TestGiteaWebhook(t *testing.T) {
	t.Setenv("WEBHOOK_SECRET", "s3cret")
	st, db := newCFRStore(t, "")
	ctx := context.Background()
	if _, err := st.SetRepoConfig(ctx, "acme/ecom", plainYAML, "t", "init"); err != nil {
		t.Fatal(err)
	}
	srv := New(st, nil, nil)
	sign := func(b []byte) string {
		m := hmac.New(sha256.New, []byte("s3cret"))
		m.Write(b)
		return hex.EncodeToString(m.Sum(nil))
	}
	push := []byte(`{"ref":"refs/tags/v1.2.0","after":"cccc3333","pusher":{"login":"ravi"},"repository":{"full_name":"acme/ecom"},
	  "head_commit":{"message":"Release","author":{"username":"ravi"}}}`)
	if rec := postHook(srv, "/api/v1/webhooks/gitea", push, map[string]string{"X-Gitea-Event": "push", "X-Gitea-Signature": "bad"}); rec.Code != 401 {
		t.Fatalf("bad signature = %d", rec.Code)
	}
	if rec := postHook(srv, "/api/v1/webhooks/gitea", push, map[string]string{"X-Forgejo-Event": "push", "X-Forgejo-Signature": sign(push)}); rec.Code != 201 {
		t.Fatalf("forgejo push = %d %s", rec.Code, rec.Body.String())
	}
	var ref, by string
	_ = db.QueryRow(ctx, `SELECT ref, triggered_by FROM pipelines WHERE repo='acme/ecom'`).Scan(&ref, &by)
	if ref != "v1.2.0" || by != "ravi" {
		t.Errorf("tag pipeline = %s by %s", ref, by)
	}
	pr := []byte(`{"action":"synchronized","number":3,"pull_request":{"title":"Cart fix","head":{"ref":"cart","sha":"dddd4444"},
	  "base":{"ref":"main"},"user":{"login":"ashwin"}},"repository":{"full_name":"acme/ecom"}}`)
	if rec := postHook(srv, "/api/v1/webhooks/gitea", pr, map[string]string{"X-Gitea-Event": "pull_request", "X-Gitea-Signature": sign(pr)}); rec.Code != 201 {
		t.Fatalf("PR = %d %s", rec.Code, rec.Body.String())
	}
}

func TestRegisterAnyGitHost(t *testing.T) {
	h := newEnforcedHarness(t)
	admin := h.login("admin@acme.io")
	for _, c := range []struct {
		provider, clone string
		want            int
	}{
		{"gitlab", "", 400}, // no reachable host in tests: clone check fails, but provider accepted
		{"gitea", "", 400},  // requires clone_url
		{"git", "", 400},    // requires clone_url
		{"nope", "https://x/y.git", 400},
	} {
		rec := h.do(admin, "POST", "/api/v1/repo-registry", map[string]any{"repo": "acme/x", "provider": c.provider, "clone_url": c.clone})
		if rec.Code != c.want {
			t.Errorf("%s: %d %s", c.provider, rec.Code, rec.Body.String())
		}
		if c.provider == "nope" && !bytes.Contains(rec.Body.Bytes(), []byte("gitea")) {
			t.Errorf("unknown provider error should list the providers: %s", rec.Body.String())
		}
		if (c.provider == "gitea" || c.provider == "git") && !bytes.Contains(rec.Body.Bytes(), []byte("requires clone_url")) {
			t.Errorf("%s without clone_url: %s", c.provider, rec.Body.String())
		}
	}
}

func TestTagPushOnEveryHost(t *testing.T) {
	gh, err := parseGitHubPush([]byte(`{"ref":"refs/tags/v2.0.0","after":"abc","repository":{"full_name":"acme/x"}}`))
	if err != nil || gh.Ref != "v2.0.0" || gh.ExtraCtx["CI_COMMIT_TAG"] != "v2.0.0" {
		t.Errorf("github tag = %+v %v", gh, err)
	}
	if br, _ := parseGitHubPush([]byte(`{"ref":"refs/heads/main","after":"abc"}`)); br.ExtraCtx != nil {
		t.Errorf("github branch push must not be a tag: %v", br.ExtraCtx)
	}
	bb, ok, err := parseBitbucketPush([]byte(`{"repository":{"full_name":"acme/x"},"push":{"changes":[{"new":{"name":"v3","type":"tag","target":{"hash":"def"}}}]}}`))
	if err != nil || !ok || bb.ExtraCtx["CI_COMMIT_TAG"] != "v3" {
		t.Errorf("bitbucket tag = %+v %v", bb, err)
	}
	gl, _, _ := parseGitLabPush([]byte(`{"object_kind":"tag_push","ref":"refs/tags/v4","checkout_sha":"aaa","project":{"path_with_namespace":"g/x"}}`))
	if gl.ExtraCtx["CI_COMMIT_TAG"] != "v4" {
		t.Errorf("gitlab tag = %+v", gl)
	}
}
