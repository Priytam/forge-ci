package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/compiler"
	"github.com/priytamjeepandey/forge-ci/internal/notify"
	"github.com/priytamjeepandey/forge-ci/internal/proto"
	"github.com/priytamjeepandey/forge-ci/internal/store"
)

func TestNotificationRoutesCRUD(t *testing.T) {
	h := newEnforcedHarness(t)
	admin := h.login("admin@acme.io")
	dev := h.login("dev@acme.io")
	h.expect(h.do(dev, "GET", "/api/v1/notification-routes", nil), 403, "")
	for body, want := range map[string]string{
		`{"name":"x","kind":"slack","target":"http://hooks","events":["pipeline_failed"]}`: "https",
		`{"name":"x","kind":"pager","target":"https://x","events":["pipeline_failed"]}`:    "kind must be",
		`{"name":"x","kind":"slack","target":"https://x","events":[]}`:                     "at least one event",
		`{"name":"x","kind":"slack","target":"https://x","events":["boom"]}`:               "unknown event",
		`{"name":"x","kind":"email","target":"not-an-email","events":["deploy_failed"]}`:   "not an email",
	} {
		var b map[string]any
		_ = json.Unmarshal([]byte(body), &b)
		if rec := h.do(admin, "POST", "/api/v1/notification-routes", b); rec.Code != 400 || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("%s → %d %s, want %q", body, rec.Code, rec.Body.String(), want)
		}
	}
	rec := h.do(admin, "POST", "/api/v1/notification-routes", map[string]any{
		"name": "ops", "kind": "webhook", "target": "https://ops.example.com/forge", "secret": "whsec", "events": []string{"pipeline_failed", "deploy_failed"}, "repo_pattern": "acme/*", "enabled": true,
	})
	h.expect(rec, 201, "")
	var route store.NotificationRoute
	_ = json.Unmarshal(rec.Body.Bytes(), &route)
	if route.Target != "" || route.Secret != "" || !route.HasTarget || !route.HasSecret || route.TargetHint != "ops.example.com/…" {
		t.Errorf("secrets leaked or hint wrong: %+v", route)
	}
	// Update without target/secret keeps them.
	h.expect(h.do(admin, "PUT", "/api/v1/notification-routes/"+strconv.FormatInt(route.ID, 10), map[string]any{
		"name": "ops2", "kind": "webhook", "events": []string{"pipeline_failed"}, "enabled": false,
	}), 200, "")
	all, _ := h.st.ListNotificationRoutes(context.Background(), true)
	if len(all) != 1 || all[0].Name != "ops2" || all[0].Target != "https://ops.example.com/forge" || all[0].Secret != "whsec" || all[0].Enabled {
		t.Errorf("after update = %+v", all)
	}
	h.expect(h.do(admin, "DELETE", "/api/v1/notification-routes/"+strconv.FormatInt(route.ID, 10), nil), 204, "")
	h.expect(h.do(admin, "DELETE", "/api/v1/notification-routes/"+strconv.FormatInt(route.ID, 10), nil), 404, "")
}

func TestApproveByEmailLink(t *testing.T) {
	h := newEnforcedHarness(t)
	ctx := context.Background()
	const repo = "acme/shop"
	_, _ = h.st.AddMember(ctx, repo, "release@acme.io", "owner")
	yml := "stages: [d]\njobs:\n  ship:\n    stage: d\n    environment: production\n    script: [x]\n"
	jobs, _ := compiler.Compile(yml, "main", compiler.SourceAPI, nil)
	p, err := h.st.CreatePipeline(ctx, proto.CreatePipelineRequest{Repo: repo, Ref: "main", SHA: "abc", Config: yml, TriggeredBy: "dev@acme.io"}, jobs, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = h.st.PromoteReadyJobs(ctx)
	_, js, _ := h.st.GetPipeline(ctx, p.ID)
	job := js[0]
	if job.Status != "blocked" {
		t.Fatalf("job = %s, want blocked on approval", job.Status)
	}
	c := h.st.Cipher()
	approve := notify.SignApprovalLink(c, job.ID, "release@acme.io", "approved", time.Now().Add(time.Hour))

	// GET is a confirmation page only: a mail scanner following it must not vote.
	get := httptest.NewRequest("GET", "/api/v1/approvals/link?t="+url.QueryEscape(approve), nil)
	rec := httptest.NewRecorder()
	h.srv.ServeHTTP(rec, get)
	body, _ := io.ReadAll(rec.Body)
	if rec.Code != 200 || !strings.Contains(string(body), "release@acme.io") || !strings.Contains(string(body), `method="post"`) {
		t.Fatalf("GET link = %d %s", rec.Code, body)
	}
	if j, _ := h.st.GetJob(ctx, job.ID); j.Status != "blocked" {
		t.Fatal("GET changed the job")
	}
	// A developer's link can't approve production (role check still applies).
	post := func(tok string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/api/v1/approvals/link", strings.NewReader("t="+url.QueryEscape(tok)))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		h.srv.ServeHTTP(rec, req)
		return rec
	}
	devLink := notify.SignApprovalLink(c, job.ID, "someone@acme.io", "approved", time.Now().Add(time.Hour))
	if rec := post(devLink); !strings.Contains(rec.Body.String(), "Not recorded") {
		t.Errorf("non-approver link: %s", rec.Body.String())
	}
	if rec := post("garbage.token"); !strings.Contains(rec.Body.String(), "not valid") {
		t.Errorf("garbage link: %s", rec.Body.String())
	}
	if rec := post(approve); !strings.Contains(rec.Body.String(), "Recorded") {
		t.Fatalf("approve link: %s", rec.Body.String())
	}
	if j, _ := h.st.GetJob(ctx, job.ID); j.Status != "created" && j.Status != "pending" {
		t.Errorf("after approval job = %s", j.Status)
	}
	// Second use: already voted.
	if rec := post(approve); !strings.Contains(rec.Body.String(), "Not recorded") {
		t.Errorf("replayed link: %s", rec.Body.String())
	}
}

func TestPipelineAndDeployEventClaims(t *testing.T) {
	h := newEnforcedHarness(t)
	ctx := context.Background()
	yml := "stages: [b, d]\njobs:\n  build:\n    stage: b\n    script: [x]\n  deploy:\n    stage: d\n    needs: [build]\n    environment: staging\n    script: [x]\n"
	run := func(sha string, buildStatus, deployStatus string) int64 {
		jobs, _ := compiler.Compile(yml, "main", compiler.SourceAPI, nil)
		p, err := h.st.CreatePipeline(ctx, proto.CreatePipelineRequest{Repo: "acme/x", Ref: "main", SHA: sha, Config: yml}, jobs, nil, false, false)
		if err != nil {
			t.Fatal(err)
		}
		_, js, _ := h.st.GetPipeline(ctx, p.ID)
		for _, j := range js {
			st := buildStatus
			if j.Name == "deploy" {
				st = deployStatus
			}
			if st == "" {
				continue
			}
			_, _ = h.db.Exec(ctx, `UPDATE jobs SET status=$2, finished_at=now() WHERE id=$1`, j.ID, st)
		}
		return p.ID
	}
	p1 := run("a1", "failed", "canceled")
	p2 := run("a2", "success", "success")
	run("a3", "success", "") // still running: not announced
	events, err := h.st.ClaimPipelineEvents(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[int64]string{}
	for _, e := range events {
		kinds[e.PipelineID] = e.Kind
	}
	if kinds[p1] != "pipeline_failed" || kinds[p2] != "pipeline_fixed" || len(events) != 2 {
		t.Errorf("events = %v", kinds)
	}
	if again, _ := h.st.ClaimPipelineEvents(ctx, 10); len(again) != 0 {
		t.Errorf("events announced twice: %v", again)
	}
	deploys, err := h.st.ClaimDeployEvents(ctx, 10)
	if err != nil || len(deploys) != 1 || deploys[0].Kind != "deploy_succeeded" || deploys[0].Environment != "staging" {
		t.Errorf("deploy events = %+v %v", deploys, err)
	}
	if again, _ := h.st.ClaimDeployEvents(ctx, 10); len(again) != 0 {
		t.Errorf("deploys announced twice: %v", again)
	}
	_ = p2
}

func TestNotifySettingsTrimAndHealth(t *testing.T) {
	h := newEnforcedHarness(t)
	admin := h.login("admin@acme.io")
	ctx := context.Background()
	// A pasted host with a leading space is trimmed, not saved broken.
	rec := h.do(admin, "PUT", "/api/v1/notification-settings?check=0", map[string]any{
		"email_enabled": true, "smtp_host": " smtp.example.com ", "smtp_port": 587, "smtp_from": " forge@example.com ", "smtp_username": " bot ",
	})
	h.expect(rec, 200, "")
	cfg, _ := h.st.GetNotifySettings(ctx, false)
	if cfg.SMTPHost != "smtp.example.com" || cfg.SMTPFrom != "forge@example.com" || cfg.SMTPUsername != "bot" {
		t.Errorf("not trimmed: %+v", cfg)
	}
	if rec := h.do(admin, "PUT", "/api/v1/notification-settings", map[string]any{"email_enabled": true, "smtp_host": "smtp .example.com", "smtp_from": "f@x"}); rec.Code != 400 || !strings.Contains(rec.Body.String(), "host name") {
		t.Errorf("host with space = %d %s", rec.Code, rec.Body.String())
	}
	// Save with the check on: an unreachable server is reported inline, the save still lands.
	rec = h.do(admin, "PUT", "/api/v1/notification-settings", map[string]any{"email_enabled": true, "smtp_host": "127.0.0.1", "smtp_port": 1, "smtp_from": "f@x.io"})
	h.expect(rec, 200, "")
	var view struct {
		Check *struct {
			OK    bool
			Error string
		} `json:"smtp_check"`
		Delivery map[string]store.DeliveryResult `json:"delivery"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &view)
	if view.Check == nil || view.Check.OK || !strings.Contains(view.Check.Error, "connect") {
		t.Errorf("smtp_check = %+v", view.Check)
	}
	// Delivery health: failures accumulate, a success resets.
	_ = h.st.RecordDeliveryResult(ctx, "email", errors.New("lookup  smtp.gmail.com: no such host"))
	_ = h.st.RecordDeliveryResult(ctx, "email", errors.New("lookup  smtp.gmail.com: no such host"))
	_ = json.Unmarshal(h.do(admin, "GET", "/api/v1/notification-settings", nil).Body.Bytes(), &view)
	if d := view.Delivery["email"]; d.OK || d.ConsecutiveFailures != 2 || !strings.Contains(d.Error, "no such host") {
		t.Errorf("delivery = %+v", d)
	}
	_ = h.st.RecordDeliveryResult(ctx, "email", nil)
	_ = json.Unmarshal(h.do(admin, "GET", "/api/v1/notification-settings", nil).Body.Bytes(), &view)
	if d := view.Delivery["email"]; !d.OK || d.ConsecutiveFailures != 0 {
		t.Errorf("after success = %+v", d)
	}
}
