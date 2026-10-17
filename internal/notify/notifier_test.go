package notify

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/secret"
	"github.com/priytamjeepandey/forge-ci/internal/store"
)

func testCipher(t *testing.T) *secret.Cipher {
	t.Setenv("FORGE_SECRET_KEY", base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
	c, err := secret.FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNotifiersPostTheRightShape(t *testing.T) {
	var got = map[string][]byte{}
	var headers = map[string]http.Header{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got[r.URL.Path] = body
		headers[r.URL.Path] = r.Header.Clone()
	}))
	defer srv.Close()
	ev := Event{Kind: EventPipelineFailed, Repo: "acme/shop", Ref: "main", SHA: "abcdef1234", PipelineID: 7,
		Title: "acme/shop: pipeline #7 on main failed", Text: "Pipeline #7 failed.", Link: "https://forge/pipelines/7",
		Fields: map[string]string{"Failed jobs": "unit, lint"}, At: time.Now()}
	for _, kind := range []string{"slack", "teams", "webhook"} {
		n, ok := Lookup(kind)
		if !ok {
			t.Fatalf("no notifier %s", kind)
		}
		if err := n.ValidateTarget("http://insecure"); err == nil {
			t.Errorf("%s accepted a plain http target", kind)
		}
		if err := n.Send(context.Background(), srv.URL+"/"+kind, "s3cret", ev); err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
	}
	var slack struct {
		Text   string           `json:"text"`
		Blocks []map[string]any `json:"blocks"`
	}
	_ = json.Unmarshal(got["/slack"], &slack)
	if slack.Text != ev.Title+"\n"+ev.Text || len(slack.Blocks) != 2 || !strings.Contains(string(got["/slack"]), "Failed jobs: unit, lint") {
		t.Errorf("slack = %s", got["/slack"])
	}
	if !strings.Contains(string(got["/teams"]), `"application/vnd.microsoft.card.adaptive"`) || !strings.Contains(string(got["/teams"]), "Action.OpenUrl") {
		t.Errorf("teams = %s", got["/teams"])
	}
	var wh Event
	_ = json.Unmarshal(got["/webhook"], &wh)
	if wh.Kind != EventPipelineFailed || wh.PipelineID != 7 || wh.Fields["Failed jobs"] != "unit, lint" {
		t.Errorf("webhook = %s", got["/webhook"])
	}
	h := headers["/webhook"]
	if h.Get("X-Forge-Event") != EventPipelineFailed || h.Get("X-Forge-Delivery") == "" ||
		h.Get("X-Forge-Signature") != "sha256="+Sign("s3cret", got["/webhook"]) {
		t.Errorf("webhook headers = %v", h)
	}
}

func TestDispatchRoutesByEventAndRepo(t *testing.T) {
	var hits []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits = append(hits, r.URL.Path) }))
	defer srv.Close()
	routes := []store.NotificationRoute{
		{Name: "all-fails", Kind: "slack", Target: srv.URL + "/all", Events: []string{EventPipelineFailed}, RepoPattern: "*", Enabled: true},
		{Name: "shop-only", Kind: "webhook", Target: srv.URL + "/shop", Events: []string{EventPipelineFailed, EventDeploySucceeded}, RepoPattern: "acme/*", Enabled: true},
		{Name: "off", Kind: "slack", Target: srv.URL + "/off", Events: []string{EventPipelineFailed}, RepoPattern: "*", Enabled: false},
		{Name: "other-event", Kind: "slack", Target: srv.URL + "/deploy", Events: []string{EventDeploySucceeded}, RepoPattern: "*", Enabled: true},
	}
	s := &Sender{}
	if err := s.Dispatch(context.Background(), routes, nil, Event{Kind: EventPipelineFailed, Repo: "globex/api"}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(hits, ",") != "/all" {
		t.Errorf("globex failure went to %v, want only /all", hits)
	}
	hits = nil
	_ = s.Dispatch(context.Background(), routes, nil, Event{Kind: EventPipelineFailed, Repo: "acme/shop"})
	if strings.Join(hits, ",") != "/all,/shop" {
		t.Errorf("acme failure went to %v", hits)
	}
}

func TestApprovalLinksRoundTrip(t *testing.T) {
	c := testCipher(t)
	tok := SignApprovalLink(c, 42, "Ravi@Acme.io", "approved", time.Now().Add(time.Hour))
	job, who, verdict, err := ParseApprovalLink(c, tok)
	if err != nil || job != 42 || who != "ravi@acme.io" || verdict != "approved" {
		t.Fatalf("parse = %d %s %s %v", job, who, verdict, err)
	}
	// Tampering with the payload (another job) breaks the signature.
	p, s, _ := strings.Cut(tok, ".")
	raw, _ := base64.RawURLEncoding.DecodeString(p)
	raw = []byte(strings.Replace(string(raw), `"j":42`, `"j":43`, 1))
	if _, _, _, err := ParseApprovalLink(c, base64.RawURLEncoding.EncodeToString(raw)+"."+s); err == nil {
		t.Error("tampered link accepted")
	}
	if _, _, _, err := ParseApprovalLink(c, SignApprovalLink(c, 42, "x@y", "approved", time.Now().Add(-time.Minute))); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Errorf("expired link: %v", err)
	}
	// A signature for another purpose never validates as a link.
	other := c.Sign("something-else", raw)
	if _, _, _, err := ParseApprovalLink(c, p+"."+base64.RawURLEncoding.EncodeToString(other)); err == nil {
		t.Error("cross-purpose signature accepted")
	}
	// No key: no links.
	if SignApprovalLink(&secret.Cipher{}, 1, "a@b", "approved", time.Now().Add(time.Hour)) != "" {
		t.Error("links signed without a key")
	}
	ev := Event{Kind: EventApprovalNeeded, JobID: 42, Approvers: []string{"ravi@acme.io", "svc:bot"}}
	ApprovalLinksFor(c, "https://forge.example.com/", &ev)
	if l, ok := ev.Actions["ravi@acme.io"]; !ok || !strings.HasPrefix(l.Approve, "https://forge.example.com/api/v1/approvals/link?t=") || l.Reject == "" {
		t.Errorf("links = %+v", ev.Actions)
	}
	if _, ok := ev.Actions["svc:bot"]; ok {
		t.Error("a non-email approver got a mail link")
	}
}
