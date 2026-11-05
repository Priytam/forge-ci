package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/priytamjeepandey/forge-ci/internal/compiler"
	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

// compareStub emulates GitHub's compare API, recording which base...head was
// asked for and answering with a fixed file list.
type compareStub struct {
	mu    sync.Mutex
	asked []string
	files []string
}

func (c *compareStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	i := strings.Index(r.URL.Path, "/compare/")
	if i < 0 {
		http.NotFound(w, r)
		return
	}
	c.mu.Lock()
	c.asked = append(c.asked, r.URL.Path[i+len("/compare/"):])
	files := c.files
	c.mu.Unlock()
	var b strings.Builder
	b.WriteString(`{"files":[`)
	for k, f := range files {
		if k > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"filename":"` + f + `"}`)
	}
	b.WriteString("]}")
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(b.String()))
}

// TestPipelineChangeSetBase: a new branch compares against the default branch
// (merge base); once the branch has a successful pipeline, against that
// pipeline's commit. API runs and unconnected repos get no change set.
func TestPipelineChangeSetBase(t *testing.T) {
	stub := &compareStub{files: []string{"web/src/App.tsx"}}
	srv := httptest.NewServer(stub)
	defer srv.Close()
	st, db := newCFRStore(t, srv.URL)
	ctx := context.Background()
	const repo = "acme/ecom"
	if err := st.RegisterRepo(ctx, proto.RepoRegistration{
		Repo: repo, Provider: "github", CloneURL: "https://github.com/acme/ecom.git",
		DefaultBranch: "main", Token: "ghp_t", ConfigSource: "registered",
	}); err != nil {
		t.Fatal(err)
	}

	cs := st.PipelineChangeSet(ctx, repo, "feature/x", "head1", compiler.SourcePush, "")
	if cs == nil || cs.Base != "main" || len(cs.Files) != 1 || cs.Files[0] != "web/src/App.tsx" {
		t.Fatalf("new branch change set = %+v", cs)
	}
	if stub.asked[0] != "main...head1" {
		t.Errorf("compare asked = %q, want main...head1", stub.asked[0])
	}

	// A successful run on the branch becomes the base.
	h := &apiHarness{t: t, st: st, db: db, srv: New(st, nil, nil)}
	p := h.seedPipeline(repo, "feature/x", plainYAML)
	if _, err := db.Exec(ctx, `UPDATE pipelines SET sha='prev7' WHERE id=$1`, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `UPDATE jobs SET status='success' WHERE pipeline_id=$1`, p.ID); err != nil {
		t.Fatal(err)
	}
	cs = st.PipelineChangeSet(ctx, repo, "feature/x", "head2", compiler.SourcePush, "")
	if cs == nil || cs.Base != "prev7" {
		t.Fatalf("change set after a green run = %+v, want base prev7", cs)
	}
	// A merge request compares against its target.
	cs = st.PipelineChangeSet(ctx, repo, "feature/x", "head3", compiler.SourceMergeRequest, "targetsha")
	if cs == nil || cs.Base != "targetsha" {
		t.Errorf("merge request change set = %+v", cs)
	}
	// The default branch's first run, API runs and unconnected repos: unknown.
	if cs := st.PipelineChangeSet(ctx, repo, "main", "h", compiler.SourcePush, ""); cs != nil {
		t.Errorf("first run on default branch = %+v, want nil", cs)
	}
	if cs := st.PipelineChangeSet(ctx, repo, "feature/x", "h", compiler.SourceAPI, ""); cs != nil {
		t.Errorf("API run = %+v, want nil", cs)
	}
	if cs := st.PipelineChangeSet(ctx, "acme/unknown", "feature/x", "h", compiler.SourcePush, ""); cs != nil {
		t.Errorf("unconnected repo = %+v, want nil", cs)
	}
	// A truncated list (300 files) is unknown, never a partial skip.
	many := make([]string, 300)
	for i := range many {
		many[i] = "f" + strings.Repeat("x", i%5) + ".go"
	}
	stub.mu.Lock()
	stub.files = many
	stub.mu.Unlock()
	if cs := st.PipelineChangeSet(ctx, repo, "feature/y", "h", compiler.SourcePush, ""); cs != nil {
		t.Errorf("truncated compare = %d files, want nil", len(cs.Files))
	}
}
