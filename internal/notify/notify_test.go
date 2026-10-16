package notify

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/priytamjeepandey/forge-ci/internal/store"
)

// fakeSMTP is a minimal plaintext SMTP server that records each message.
type fakeSMTP struct {
	ln   net.Listener
	mu   sync.Mutex
	mail []string // "rcpt\n<data>"
	auth bool
}

func newFakeSMTP(t *testing.T) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{ln: ln}
	go f.serve()
	t.Cleanup(func() { ln.Close() })
	return f
}

func (f *fakeSMTP) port() int { return f.ln.Addr().(*net.TCPAddr).Port }

func (f *fakeSMTP) serve() {
	for {
		c, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.handle(c)
	}
}

func (f *fakeSMTP) handle(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	say := func(s string) { _, _ = io.WriteString(c, s+"\r\n") }
	say("220 fake ESMTP")
	var rcpt string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			say("250-fake")
			say("250 AUTH PLAIN")
		case strings.HasPrefix(cmd, "AUTH"):
			f.mu.Lock()
			f.auth = true
			f.mu.Unlock()
			say("235 ok")
		case strings.HasPrefix(cmd, "MAIL"):
			say("250 ok")
		case strings.HasPrefix(cmd, "RCPT"):
			rcpt = strings.Trim(strings.TrimPrefix(strings.TrimSpace(line), "RCPT TO:"), "<>")
			say("250 ok")
		case cmd == "DATA":
			say("354 go")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				b.WriteString(l)
			}
			f.mu.Lock()
			f.mail = append(f.mail, rcpt+"\n"+b.String())
			f.mu.Unlock()
			say("250 queued")
		case cmd == "QUIT":
			say("221 bye")
			return
		default:
			say("250 ok")
		}
	}
}

func (f *fakeSMTP) messages() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.mail...)
}

var sample = store.ApprovalNotice{
	JobID: 9, JobName: "deploy_prod", PipelineID: 4, Repo: "acme/ecom", Ref: "refs/heads/main",
	SHA: "abcdef1234567890", Environment: "production", TriggeredBy: "ravi@acme.io",
	CommitMessage: "Hotfix checkout\n\nmore", RequiredApprovals: 2, ApproverRoles: []string{"owner"},
	Approvers: []string{"ashwin@acme.io", "harish@acme.io", "not-an-email"},
}

func TestApprovalMessage(t *testing.T) {
	m := ApprovalMessage(sample, "https://forge.acme.io/")
	if m.Link != "https://forge.acme.io/jobs/9?pipeline=4" {
		t.Errorf("link = %s", m.Link)
	}
	for _, want := range []string{"deploy_prod in acme/ecom", "production", "main @ abcdef12", "Hotfix checkout",
		"Needs 2 approvals from owner", "harish@acme.io", m.Link} {
		if !strings.Contains(m.Text, want) {
			t.Errorf("message missing %q:\n%s", want, m.Text)
		}
	}
}

func TestDeliverSlackAndEmail(t *testing.T) {
	var got map[string]any
	slack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusOK)
	}))
	defer slack.Close()
	smtpSrv := newFakeSMTP(t)

	cfg := store.NotifySettings{
		SlackWebhookURL: slack.URL, EmailEnabled: true, SMTPHost: "127.0.0.1",
		SMTPPort: smtpSrv.port(), SMTPUsername: "forge", SMTPPassword: "pw", SMTPFrom: "forge@acme.io",
	}
	m := ApprovalMessage(sample, "https://forge.acme.io")
	if err := (&Sender{}).Deliver(context.Background(), cfg, sample.Approvers, m); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if !strings.Contains(got["text"].(string), "deploy_prod") {
		t.Errorf("slack payload = %v", got)
	}
	mails := smtpSrv.messages()
	if len(mails) != 2 {
		t.Fatalf("emails sent = %d, want 2 (non-email approver skipped): %v", len(mails), mails)
	}
	if !strings.HasPrefix(mails[0], "ashwin@acme.io\n") || !strings.Contains(mails[0], "Subject: [Forge] acme/ecom") {
		t.Errorf("first email = %q", mails[0])
	}
	if !smtpSrv.auth {
		t.Error("SMTP auth was not attempted with a username set")
	}

	// A failing channel is reported, the other still delivers.
	cfg.SlackWebhookURL = "http://127.0.0.1:1/nothing"
	err := (&Sender{}).Deliver(context.Background(), cfg, []string{"x@acme.io"}, m)
	if err == nil || !strings.Contains(err.Error(), "slack") {
		t.Errorf("want a slack error, got %v", err)
	}
	if n := len(smtpSrv.messages()); n != 3 {
		t.Errorf("email should still send when slack fails: %d messages", n)
	}
}
