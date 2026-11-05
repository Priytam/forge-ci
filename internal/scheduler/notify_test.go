package scheduler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/priytamjeepandey/forge-ci/internal/compiler"
	"github.com/priytamjeepandey/forge-ci/internal/notify"
	"github.com/priytamjeepandey/forge-ci/internal/proto"
	"github.com/priytamjeepandey/forge-ci/internal/store"
)

// TestApprovalWaitNotifiesApproversOnce: a job that starts waiting for
// production approval produces one Slack message naming the eligible
// approvers (owners, minus the author when self-approval is off) with a link
// to the job, and is never notified twice.
func TestApprovalWaitNotifiesApproversOnce(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	t.Setenv("FRONTEND_URL", "https://forge.acme.io")

	var mu sync.Mutex
	var texts []string
	slack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Text string `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		texts = append(texts, body.Text)
		mu.Unlock()
	}))
	defer slack.Close()
	if err := st.SaveNotifySettings(ctx, store.NotifySettings{SlackWebhookURL: slack.URL}); err != nil {
		t.Fatal(err)
	}

	const repo = "acme/ecom"
	for _, m := range []struct{ user, role string }{
		{"harish@acme.io", "owner"}, {"ashwin@acme.io", "owner"}, {"ravi@acme.io", "owner"}, {"dev@acme.io", "developer"},
	} {
		if _, err := st.AddMember(ctx, repo, m.user, m.role); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.UpsertProtectedEnv(ctx, proto.ProtectedEnvironment{
		Repo: repo, Name: "production", RequiredApprovals: 2, ApprovalTimeoutHours: 24,
		ApproverRoles: []string{"owner"}, AllowSelfApproval: false,
	}); err != nil {
		t.Fatal(err)
	}
	yml := `
stages: [deploy]
jobs:
  deploy_prod:
    stage: deploy
    environment: production
    script: [./deploy]
`
	jobs, err := compiler.Compile(yml, "main", compiler.SourceAPI, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreatePipeline(ctx, proto.CreatePipelineRequest{
		Repo: repo, Ref: "main", SHA: "abcdef1234567890", Config: yml, TriggeredBy: "ravi@acme.io",
	}, jobs, nil, false, false); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PromoteReadyJobs(ctx); err != nil {
		t.Fatal(err)
	}

	sc := &Scheduler{store: st, sender: &notify.Sender{}}
	sc.NotifyApprovalsOnce(ctx)
	sc.NotifyApprovalsOnce(ctx) // must not repeat

	mu.Lock()
	defer mu.Unlock()
	if len(texts) != 1 {
		t.Fatalf("slack messages = %d, want exactly 1: %v", len(texts), texts)
	}
	msg := texts[0]
	for _, want := range []string{"deploy_prod in acme/ecom", "production", "Needs 2 approvals from owner",
		"ashwin@acme.io, harish@acme.io", "https://forge.acme.io/jobs/"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "Can approve: ashwin@acme.io, harish@acme.io, ravi") || strings.Contains(msg, "dev@acme.io") {
		t.Errorf("author (self-approval off) or non-approver listed:\n%s", msg)
	}
}

// TestNoChannelClaimsNothing: with nothing configured, waits stay unclaimed,
// so enabling Slack later still reaches jobs blocked in the last hour.
func TestNoChannelClaimsNothing(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	yml := "stages: [d]\njobs:\n  ship:\n    stage: d\n    environment: production\n    script: [x]\n"
	jobs, _ := compiler.Compile(yml, "main", compiler.SourceAPI, nil)
	if _, err := st.CreatePipeline(ctx, proto.CreatePipelineRequest{Repo: "r", Ref: "main", SHA: "s", Config: yml}, jobs, nil, false, false); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PromoteReadyJobs(ctx); err != nil {
		t.Fatal(err)
	}
	sc := &Scheduler{store: st, sender: &notify.Sender{}}
	sc.NotifyApprovalsOnce(ctx)
	n, err := st.ClaimApprovalNotices(ctx, 10, []string{"admin@acme.io"})
	if err != nil {
		t.Fatal(err)
	}
	if len(n) != 1 || len(n[0].Approvers) != 1 || n[0].Approvers[0] != "admin@acme.io" {
		t.Errorf("unclaimed wait on a member-less repo should go to the admins: %+v", n)
	}
}
