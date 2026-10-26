// Package vcstest is the provider conformance suite: one set of tests every
// code-review plugin must pass, run against a fake host. A provider is "done"
// when RunChangeSource is green for it — that is what makes the experience
// identical on every host. The suite branches on the plugin's declared
// capabilities, never on its name.
package vcstest

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/vcs"
)

// Host is a fake git host the suite drives.
type Host interface {
	// Repo is how Forge would have the repo registered.
	Repo() vcs.RepoRef
	// Open creates an open change and returns its id plus the copy Forge
	// would hold before its first refresh (native changes need the branches).
	Open(t *testing.T, title string) (id string, stored *vcs.Change)
	// Push moves the change's head and returns the new head sha.
	Push(t *testing.T, id string) string
	// Close closes the change without merging.
	Close(t *testing.T, id string)
	// Review records a verdict on the host (hosts with a change API only).
	Review(t *testing.T, id, reviewer, verdict string)
	// Delivery is the webhook the host would send for kind ("change" or
	// "review") on the change; ok=false when the host sends none.
	Delivery(t *testing.T, id, kind string) (hdr http.Header, body []byte, ok bool)
	// Unrelated is a delivery that concerns no change (a push), or ok=false.
	Unrelated(t *testing.T) (hdr http.Header, body []byte, ok bool)
	// RateLimit makes following responses carry these rate-limit headers.
	RateLimit(remaining, limit int, reset time.Time)
	// RejectNext refuses the next request with status and Retry-After.
	RejectNext(status int, retryAfter time.Duration)
	// Head is the change's current head sha on the host.
	Head(t *testing.T, id string) string
	// Protect makes the host require these status contexts before merging
	// (branch protection). Hosts without status checks ignore it.
	Protect(contexts ...string)
	// Check is the state of a status context on sha ("" when none).
	Check(t *testing.T, sha, context string) string
	// HostMerge is a person clicking Merge in the host's own UI.
	HostMerge(t *testing.T, id string) error
	// HostThread is someone commenting on a line in the host's UI; it
	// returns the thread's id (hosts with threads only).
	HostThread(t *testing.T, id, login, body string) string
	// CommentsOn lists every comment on the change as "login: body".
	CommentsOn(t *testing.T, id string) []string
}

// Harness names the plugin under test and builds a fresh fake host per test.
type Harness struct {
	Provider string
	New      func(t *testing.T) Host
}

// recorder captures what the client reported, per call.
type recorder struct {
	mu    sync.Mutex
	calls []vcs.CallInfo
}

func (r *recorder) on(_ context.Context, info vcs.CallInfo) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, info)
}

func (r *recorder) all() []vcs.CallInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]vcs.CallInfo(nil), r.calls...)
}

func newClient() (*vcs.ChangeClient, *recorder) {
	rec := &recorder{}
	c := vcs.NewChangeClient()
	c.OnCall = rec.on
	return c, rec
}

// RunChangeSource runs the conformance suite for one plugin.
func RunChangeSource(t *testing.T, h Harness) {
	src := vcs.ChangeSourceFor(h.Provider)
	if src == nil {
		t.Fatalf("%s: no ChangeSource registered", h.Provider)
	}
	caps := vcs.CapabilitiesOf(h.Provider)
	if caps.Changes == caps.NativeChanges {
		t.Fatalf("%s: capabilities must declare exactly one of changes / native_changes: %+v", h.Provider, caps)
	}
	ctx := context.Background()

	t.Run("event normalisation", func(t *testing.T) {
		host := h.New(t)
		id, _ := host.Open(t, "Normalise me")
		repo := host.Repo()
		for _, kind := range []string{"change", "review", "comment"} {
			hdr, body, ok := host.Delivery(t, id, kind)
			if !ok {
				continue
			}
			evs, err := src.NormalizeEvent(hdr, body)
			if err != nil || len(evs) != 1 {
				t.Fatalf("%s delivery: events = %+v, err = %v", kind, evs, err)
			}
			ev := evs[0]
			if ev.Repo != repo.Repo || ev.ChangeID != id || ev.Kind != kind {
				t.Errorf("%s delivery: event = %+v, want repo %s change %s", kind, ev, repo.Repo, id)
			}
			if ev.Snapshot != nil && (ev.Snapshot.ID != id || ev.Snapshot.State == "" || ev.Snapshot.HeadSHA == "") {
				t.Errorf("%s delivery: incomplete snapshot %+v", kind, ev.Snapshot)
			}
			// Idempotent replay: a redelivery normalises to exactly the same
			// events, so storing it twice changes nothing.
			again, err := src.NormalizeEvent(hdr, body)
			if err != nil || !reflect.DeepEqual(evs, again) {
				t.Errorf("%s delivery: replay differs: %+v vs %+v (%v)", kind, evs, again, err)
			}
		}
		if hdr, body, ok := host.Unrelated(t); ok {
			if evs, err := src.NormalizeEvent(hdr, body); err != nil || len(evs) != 0 {
				t.Errorf("unrelated delivery: events = %+v, err = %v", evs, err)
			}
		}
	})

	t.Run("get change", func(t *testing.T) {
		host := h.New(t)
		c, _ := newClient()
		repo := host.Repo()
		id, stored := host.Open(t, "Add the thing")
		ch, err := src.GetChange(ctx, c, repo, id, stored)
		if err != nil {
			t.Fatalf("GetChange: %v", err)
		}
		if ch.ID != id || ch.State != "open" || ch.HeadSHA == "" || ch.SourceBranch == "" || ch.TargetBranch == "" {
			t.Fatalf("GetChange = %+v", ch)
		}
		head := host.Push(t, id)
		ch2, err := src.GetChange(ctx, c, repo, id, ch)
		if err != nil || ch2.HeadSHA != head {
			t.Fatalf("after push: head = %q (err %v), want %q", ch2.HeadSHA, err, head)
		}
		if caps.Changes {
			host.Review(t, id, "reviewer1", "approve")
			ch3, err := src.GetChange(ctx, c, repo, id, ch2)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, r := range ch3.Reviews {
				found = found || (r.Reviewer == "reviewer1" && r.Verdict == "approve")
			}
			if !found {
				t.Errorf("review not returned: %+v", ch3.Reviews)
			}
			ch2 = ch3
		}
		host.Close(t, id)
		ch4, err := src.GetChange(ctx, c, repo, id, ch2)
		if err != nil || ch4.State != "closed" {
			t.Errorf("after close: state = %+v (err %v), want closed", ch4, err)
		}
	})

	t.Run("unknown change", func(t *testing.T) {
		host := h.New(t)
		c, _ := newClient()
		_, err := src.GetChange(ctx, c, host.Repo(), "987654", nil)
		if !errors.Is(err, vcs.ErrChangeNotFound) {
			t.Errorf("GetChange(unknown) err = %v, want ErrChangeNotFound", err)
		}
	})

	t.Run("conditional list", func(t *testing.T) {
		host := h.New(t)
		c, rec := newClient()
		repo := host.Repo()
		id, _ := host.Open(t, "Listed")
		refs, v, complete, err := src.ListOpenChanges(ctx, c, repo, vcs.Validators{})
		if caps.NativeChanges {
			if !errors.Is(err, vcs.ErrNoChangeAPI) {
				t.Errorf("native-change host: ListOpenChanges err = %v, want ErrNoChangeAPI", err)
			}
			return
		}
		if err != nil || !complete || len(refs) != 1 || refs[0].ID != id || refs[0].HeadSHA == "" {
			t.Fatalf("list = %+v complete=%v err=%v", refs, complete, err)
		}
		if v == (vcs.Validators{}) {
			t.Fatal("list returned no validators: reconcile could never be conditional")
		}
		if _, _, _, err := src.ListOpenChanges(ctx, c, repo, v); !errors.Is(err, vcs.ErrNotModified) {
			t.Fatalf("unchanged list err = %v, want ErrNotModified", err)
		}
		calls := rec.all()
		if last := calls[len(calls)-1]; last.Result != "not_modified" {
			t.Errorf("unchanged list reported as %q, want not_modified", last.Result)
		}
		head := host.Push(t, id)
		refs, _, _, err = src.ListOpenChanges(ctx, c, repo, v)
		if err != nil || len(refs) != 1 || refs[0].HeadSHA != head {
			t.Errorf("after push: list = %+v err = %v, want head %s", refs, err, head)
		}
	})

	t.Run("read path refused", func(t *testing.T) {
		host := h.New(t)
		c, rec := newClient()
		id, stored := host.Open(t, "Never on a page")
		_, err := src.GetChange(vcs.ReadPath(ctx), c, host.Repo(), id, stored)
		if !errors.Is(err, vcs.ErrReadPath) {
			t.Fatalf("GetChange on the read path err = %v, want ErrReadPath", err)
		}
		for _, call := range rec.all() {
			if call.Result != "refused" || !call.ReadPath {
				t.Errorf("read-path call reached the host: %+v", call)
			}
		}
	})

	if caps.StatusChecks || len(caps.MergeMethods) > 0 {
		runWriter(t, h, caps)
	}
	if caps.Threads {
		runThreads(t, h, caps)
	} else if vcs.ThreadWriterFor(h.Provider) != nil {
		t.Errorf("%s has a ThreadWriter but does not declare threads", h.Provider)
	}
	if !caps.Changes {
		return // rate limits only exist where there is an API
	}
	t.Run("rate-limit headers honoured", func(t *testing.T) {
		host := h.New(t)
		c, rec := newClient()
		id, stored := host.Open(t, "Budgeted")
		reset := time.Now().Add(30 * time.Minute).Truncate(time.Second)
		host.RateLimit(42, 5000, reset)
		if _, err := src.GetChange(ctx, c, host.Repo(), id, stored); err != nil {
			t.Fatal(err)
		}
		calls := rec.all()
		got := calls[len(calls)-1].Rate
		if got == nil || got.Remaining != 42 || got.Limit != 5000 || !got.Reset.Equal(reset) {
			t.Errorf("rate info = %+v, want 42/5000 reset %v", got, reset)
		}
		host.RejectNext(http.StatusTooManyRequests, 30*time.Second)
		_, err := src.GetChange(ctx, c, host.Repo(), id, stored)
		var rl *vcs.RateLimitedError
		if !errors.As(err, &rl) || rl.RetryAfter != 30*time.Second {
			t.Errorf("rate-limited GetChange err = %v, want RateLimitedError(30s)", err)
		}
	})
}

// runWriter is the write half of the suite: the merge gate is published and
// enforced on the host, and Forge merges only the head it approved.
func runWriter(t *testing.T, h Harness, caps vcs.Capabilities) {
	ctx := context.Background()
	w := vcs.ChangeWriterFor(h.Provider)
	if w == nil {
		t.Fatalf("%s declares status checks or merge methods but has no ChangeWriter", h.Provider)
	}
	if !caps.StatusChecks || len(caps.MergeMethods) == 0 {
		t.Fatalf("%s: status checks and merging ship together (the gate must be enforceable): %+v", h.Provider, caps)
	}

	t.Run("gate blocks a merge on the host until it passes", func(t *testing.T) {
		host := h.New(t)
		c, _ := newClient()
		repo := host.Repo()
		host.Protect(vcs.GateContext)
		id, _ := host.Open(t, "Gated")
		head := host.Head(t, id)
		if err := w.PublishCheck(ctx, c, repo, head, vcs.CheckResult{State: "pending", Description: "1 of 2 approvals"}); err != nil {
			t.Fatal(err)
		}
		if got := host.Check(t, head, vcs.GateContext); got != "pending" {
			t.Fatalf("published gate = %q, want pending", got)
		}
		if err := host.HostMerge(t, id); err == nil {
			t.Fatal("the host merged while the gate was pending")
		}
		// Re-publishing the same result is harmless (outbox retries).
		for i := 0; i < 2; i++ {
			if err := w.PublishCheck(ctx, c, repo, head, vcs.CheckResult{State: "success", Description: "All merge rules pass"}); err != nil {
				t.Fatal(err)
			}
		}
		if err := host.HostMerge(t, id); err != nil {
			t.Errorf("the host refused a merge after the gate passed: %v", err)
		}
	})

	t.Run("merge with expected head", func(t *testing.T) {
		host := h.New(t)
		c, _ := newClient()
		repo := host.Repo()
		host.Protect(vcs.GateContext)
		id, stored := host.Open(t, "Merge me")
		approved := host.Head(t, id)
		if err := w.PublishCheck(ctx, c, repo, approved, vcs.CheckResult{State: "success"}); err != nil {
			t.Fatal(err)
		}
		head := host.Push(t, id) // someone pushes after approval
		_, err := w.Merge(ctx, c, repo, id, vcs.MergeOpts{Method: caps.MergeMethods[0], ExpectedHead: approved})
		if !errors.Is(err, vcs.ErrHeadMoved) {
			t.Fatalf("merge of a stale head err = %v, want ErrHeadMoved", err)
		}
		_, err = w.Merge(ctx, c, repo, id, vcs.MergeOpts{Method: caps.MergeMethods[0], ExpectedHead: head})
		if !errors.Is(err, vcs.ErrNotMergeable) {
			t.Fatalf("merge without a passing gate err = %v, want ErrNotMergeable", err)
		}
		if err := w.PublishCheck(ctx, c, repo, head, vcs.CheckResult{State: "success"}); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Merge(ctx, c, repo, id, vcs.MergeOpts{Method: caps.MergeMethods[0], ExpectedHead: head}); err != nil {
			t.Fatalf("merge: %v", err)
		}
		src := vcs.ChangeSourceFor(h.Provider)
		ch, err := src.GetChange(ctx, c, repo, id, stored)
		if err != nil || ch.State != "merged" {
			t.Errorf("after merge: %+v %v, want merged", ch, err)
		}
	})

	t.Run("writes refused on the read path", func(t *testing.T) {
		host := h.New(t)
		c, rec := newClient()
		id, _ := host.Open(t, "x")
		err := w.PublishCheck(vcs.ReadPath(ctx), c, host.Repo(), host.Head(t, id), vcs.CheckResult{State: "success"})
		if !errors.Is(err, vcs.ErrReadPath) || host.Check(t, host.Head(t, id), vcs.GateContext) != "" {
			t.Errorf("read-path publish err = %v", err)
		}
		for _, call := range rec.all() {
			if call.Result != "refused" {
				t.Errorf("read-path write reached the host: %+v", call)
			}
		}
	})
}

// runThreads is the comment half: a comment posted by Forge comes back from
// the host with its marker (so Forge recognises its own and never stores it
// twice), replies land in the thread, host comments are read, and resolve
// works both ways.
func runThreads(t *testing.T, h Harness, caps vcs.Capabilities) {
	ctx := context.Background()
	w := vcs.ThreadWriterFor(h.Provider)
	src := vcs.ChangeSourceFor(h.Provider)
	if w == nil || !caps.Resolve {
		t.Fatalf("%s declares threads but has no ThreadWriter or no resolve", h.Provider)
	}
	find := func(ch *vcs.Change, uid string) (vcs.Thread, vcs.Comment, bool) {
		for _, th := range ch.Threads {
			for _, c := range th.Comments {
				if _, got := vcs.Unmark(c.Body); got == uid {
					return th, c, true
				}
			}
		}
		return vcs.Thread{}, vcs.Comment{}, false
	}

	t.Run("comment round trip", func(t *testing.T) {
		host := h.New(t)
		c, _ := newClient()
		repo := host.Repo()
		id, stored := host.Open(t, "Discuss")
		head := host.Head(t, id)
		cid, err := w.PostComment(ctx, c, repo, id, vcs.CommentRequest{Body: vcs.Mark("Why not a map?", "aa11bb22"),
			Anchor: &vcs.ThreadAnchor{Path: "app.go", Side: "new", Line: 3, CommitSHA: head}})
		if err != nil || cid == "" {
			t.Fatalf("new thread: %q %v", cid, err)
		}
		if _, err := w.PostComment(ctx, c, repo, id, vcs.CommentRequest{Body: vcs.Mark("Good point", "cc33dd44"), ReplyTo: cid}); err != nil {
			t.Fatalf("reply: %v", err)
		}
		if _, err := w.PostComment(ctx, c, repo, id, vcs.CommentRequest{Body: vcs.Mark("Ready for another look", "ee55ff66")}); err != nil {
			t.Fatalf("conversation: %v", err)
		}
		ch, err := src.GetChange(ctx, c, repo, id, stored)
		if err != nil {
			t.Fatal(err)
		}
		th, cm, ok := find(ch, "aa11bb22")
		if !ok || cm.ExternalID != cid || th.Path != "app.go" || th.Line != 3 || th.Side != "new" || len(th.Comments) != 2 {
			t.Fatalf("thread = %+v (found %v)", th, ok)
		}
		if body, _ := vcs.Unmark(cm.Body); body != "Why not a map?" {
			t.Errorf("body = %q", body)
		}
		if conv, _, ok := find(ch, "ee55ff66"); !ok || conv.ExternalID != vcs.ConversationThread || conv.Path != "" {
			t.Errorf("conversation comment = %+v %v", conv, ok)
		}
		if err := w.ResolveThread(ctx, c, repo, th.ExternalID, true); err != nil {
			t.Fatal(err)
		}
		ch, _ = src.GetChange(ctx, c, repo, id, stored)
		if th, _, _ := find(ch, "aa11bb22"); !th.Resolved {
			t.Error("resolve did not reach the host")
		}
		if err := w.ResolveThread(ctx, c, repo, th.ExternalID, false); err != nil {
			t.Fatal(err)
		}
		ch, _ = src.GetChange(ctx, c, repo, id, stored)
		if th, _, _ := find(ch, "aa11bb22"); th.Resolved {
			t.Error("unresolve did not reach the host")
		}
	})

	t.Run("host comments are read", func(t *testing.T) {
		host := h.New(t)
		c, _ := newClient()
		id, stored := host.Open(t, "x")
		tid := host.HostThread(t, id, "reviewer1", "Typo here")
		ch, err := src.GetChange(ctx, c, host.Repo(), id, stored)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, th := range ch.Threads {
			if th.ExternalID == tid && len(th.Comments) == 1 && th.Comments[0].Author == "reviewer1" && th.Comments[0].Body == "Typo here" {
				found = true
			}
		}
		if !found {
			t.Errorf("host thread %s not read: %+v", tid, ch.Threads)
		}
	})

	t.Run("post review", func(t *testing.T) {
		host := h.New(t)
		c, _ := newClient()
		id, stored := host.Open(t, "x")
		if _, err := w.PostReview(ctx, c, host.Repo(), id, "approve", "LGTM", host.Head(t, id)); err != nil {
			t.Fatal(err)
		}
		ch, _ := src.GetChange(ctx, c, host.Repo(), id, stored)
		if len(ch.Reviews) != 1 || ch.Reviews[0].Verdict != "approve" {
			t.Errorf("reviews = %+v", ch.Reviews)
		}
	})
}
