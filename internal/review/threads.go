package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/store"
	"github.com/priytamjeepandey/forge-ci/internal/vcs"
)

// Comments, resolves and reviews made in Forge reach the host through the
// outbox. Each is written as its author when they linked their host account
// (their own token and rate budget), else by Forge's bot with "via Forge"
// and their name. A comment carries Forge's hidden marker, so a retry after
// a lost response finds it on the host instead of posting it twice, and the
// host's echo is matched to Forge's copy.

// Outbox kinds for threads.
const (
	KindPostComment   = "post_comment"
	KindResolveThread = "resolve_thread"
	KindPostReview    = "post_review"
)

// replyWait bounds how long a reply waits for its thread to reach the host.
const replyWait = 10 * time.Minute

// CommentPayload names the comment to post.
type CommentPayload struct {
	CommentID int64 `json:"comment_id"`
}

// ResolvePayload resolves or reopens a thread on the host.
type ResolvePayload struct {
	ThreadID int64 `json:"thread_id"`
}

// ReviewPayload submits a Forge verdict as the reviewer's own review.
type ReviewPayload struct {
	Verdict  string `json:"verdict"`
	Body     string `json:"body"`
	HeadSHA  string `json:"head_sha"`
	Reviewer string `json:"reviewer"`
}

// writeTarget is the connection a write goes out on: the author's own token
// when they linked their account, else the repo's (bot) credential.
func (s *Service) writeTarget(ctx context.Context, repo, provider, email string) (ref vcs.RepoRef, cred string, asUser bool, err error) {
	ref, cred, found, err := s.store.ReviewTarget(ctx, repo)
	if err != nil || !found {
		if err == nil {
			err = errors.New("repo is not connected")
		}
		return ref, "", false, err
	}
	if email != "" {
		login, token, ok, err := s.store.UserHostToken(ctx, email, provider)
		if err != nil {
			return ref, "", false, err
		}
		if ok {
			ref.Token = token
			return ref, provider + ":user:" + login, true, nil
		}
	}
	return ref, cred, false, nil
}

func (s *Service) sendThreadWrite(ctx context.Context, o store.OutboxItem) {
	w := vcs.ThreadWriterFor(providerOf(ctx, s, o))
	if w == nil {
		s.finishWrite(ctx, o, "failed", nil, errors.New("this host takes no comments from Forge"), time.Time{}, true)
		return
	}
	switch o.Kind {
	case KindPostComment:
		s.postComment(ctx, o, w)
	case KindResolveThread:
		s.resolveThread(ctx, o, w)
	case KindPostReview:
		s.postReview(ctx, o, w)
	}
}

func providerOf(ctx context.Context, s *Service, o store.OutboxItem) string {
	p, _, _ := s.store.RegisteredProvider(ctx, o.Repo)
	return p
}

// budget takes a token for o from cred's bucket, deferring o when empty.
func (s *Service) budget(ctx context.Context, o store.OutboxItem, cred, provider string) bool {
	ok, wait, err := s.store.AcquireProviderBudget(ctx, cred, provider, o.Class)
	if err != nil || !ok {
		s.finishWrite(ctx, o, "pending", nil, errBudget, time.Now().Add(max(wait, time.Second)), false)
		return false
	}
	return true
}

// alreadyOnHost re-reads the change and reports whether the comment with
// uid is there (a previous attempt posted it and lost the answer).
func (s *Service) alreadyOnHost(ctx context.Context, c *store.CommentToSend, ref vcs.RepoRef, cctx context.Context) (string, bool) {
	src := vcs.ChangeSourceFor(c.Provider)
	if src == nil {
		return "", false
	}
	prev, _ := s.store.StoredChange(ctx, c.Repo, c.Number)
	ch, err := src.GetChange(cctx, s.client, ref, c.Number, prev)
	if err != nil {
		return "", false
	}
	_, _, _ = s.store.ApplyChange(ctx, c.Repo, c.Provider, ch)
	for _, th := range ch.Threads {
		for _, cm := range th.Comments {
			if _, uid := vcs.Unmark(cm.Body); uid == c.UID {
				return cm.ExternalID, true
			}
		}
	}
	return "", false
}

func (s *Service) postComment(ctx context.Context, o store.OutboxItem, w vcs.ThreadWriter) {
	var p CommentPayload
	_ = json.Unmarshal(o.Payload, &p)
	c, err := s.store.CommentForSend(ctx, p.CommentID)
	if errors.Is(err, store.ErrNotFound) {
		s.finishWrite(ctx, o, "done", map[string]string{"note": "comment deleted"}, nil, time.Time{}, true)
		return
	}
	if err != nil {
		s.finishWrite(ctx, o, "pending", nil, err, time.Now().Add(10*time.Second), true)
		return
	}
	if c.State == "sent" {
		s.finishWrite(ctx, o, "done", map[string]string{"external_id": c.ExternalID}, nil, time.Time{}, true)
		return
	}
	ref, cred, asUser, err := s.writeTarget(ctx, c.Repo, c.Provider, c.AuthorEmail)
	if err != nil {
		s.finishWrite(ctx, o, "pending", nil, err, time.Now().Add(30*time.Second), true)
		return
	}
	req := vcs.CommentRequest{}
	switch {
	case c.Path != "" && c.IsRoot:
		req.Anchor = &vcs.ThreadAnchor{Path: c.Path, Side: c.Side, Line: c.Line, StartLine: c.StartLine, CommitSHA: c.CommitSHA}
	case c.Path != "":
		if c.RootExt == "" {
			// The thread's first comment is not on the host yet.
			if time.Since(o.CreatedAt) > replyWait {
				_ = s.store.MarkCommentFailed(ctx, c, "the thread never reached the host")
				s.finishWrite(ctx, o, "failed", nil, errors.New("the thread never reached the host"), time.Time{}, true)
				return
			}
			s.finishWrite(ctx, o, "pending", nil, nil, time.Now().Add(3*time.Second), false)
			return
		}
		req.ReplyTo = c.RootExt
	}
	if !s.budget(ctx, o, cred, ref.Provider) {
		return
	}
	cctx := s.callCtx(ctx, o.Class, cred)
	if o.Attempts > 0 {
		if ext, ok := s.alreadyOnHost(ctx, c, ref, cctx); ok {
			_ = s.store.MarkCommentSent(ctx, c, ext)
			s.finishWrite(ctx, o, "done", map[string]string{"external_id": ext, "note": "posted by an earlier attempt"}, nil, time.Time{}, true)
			return
		}
	}
	body := c.Body
	if !asUser {
		body += "\n\n— " + c.Author + " via Forge"
	}
	req.Body = vcs.Mark(body, c.UID)
	ext, err := w.PostComment(cctx, s.client, ref, c.Number, req)
	if err != nil {
		if s.retryOrFail(ctx, o, err, cred, ref.Provider) {
			_ = s.store.MarkCommentFailed(ctx, c, err.Error())
		}
		return
	}
	if err := s.store.MarkCommentSent(ctx, c, ext); err != nil {
		slog.Error("code review: mark comment sent", "err", err)
	}
	s.finishWrite(ctx, o, "done", map[string]string{"external_id": ext}, nil, time.Time{}, true)
	// The host gives a new thread its id on the next read; webhooks would do
	// it too, this makes resolve-right-after-commenting work without one.
	if req.Anchor != nil {
		_ = s.store.RequestChangeRefresh(ctx, c.Repo, c.Provider, c.Number, store.PriorityWebhook, debounce, false)
	}
}

func (s *Service) resolveThread(ctx context.Context, o store.OutboxItem, w vcs.ThreadWriter) {
	var p ResolvePayload
	_ = json.Unmarshal(o.Payload, &p)
	ext, resolved, _, repo, provider, err := s.store.ThreadForResolve(ctx, p.ThreadID)
	if errors.Is(err, store.ErrNotFound) {
		s.finishWrite(ctx, o, "done", map[string]string{"note": "thread deleted"}, nil, time.Time{}, true)
		return
	}
	if err != nil {
		s.finishWrite(ctx, o, "pending", nil, err, time.Now().Add(10*time.Second), true)
		return
	}
	if ext == "" || ext == vcs.ConversationThread {
		if time.Since(o.CreatedAt) > replyWait {
			s.finishWrite(ctx, o, "failed", nil, errors.New("the thread never reached the host"), time.Time{}, true)
			return
		}
		s.finishWrite(ctx, o, "pending", nil, nil, time.Now().Add(3*time.Second), false)
		return
	}
	ref, cred, _, err := s.writeTarget(ctx, repo, provider, o.RequestedBy)
	if err != nil {
		s.finishWrite(ctx, o, "pending", nil, err, time.Now().Add(30*time.Second), true)
		return
	}
	if !s.budget(ctx, o, cred, ref.Provider) {
		return
	}
	// Resolving is idempotent on the host; the thread's current flag wins
	// (several toggles while queued send only the last).
	if err := w.ResolveThread(s.callCtx(ctx, o.Class, cred), s.client, ref, ext, resolved); err != nil {
		s.retryOrFail(ctx, o, err, cred, ref.Provider)
		return
	}
	_ = s.store.ClearResolvePending(ctx, p.ThreadID)
	s.finishWrite(ctx, o, "done", map[string]bool{"resolved": resolved}, nil, time.Time{}, true)
}

func (s *Service) postReview(ctx context.Context, o store.OutboxItem, w vcs.ThreadWriter) {
	var p ReviewPayload
	_ = json.Unmarshal(o.Payload, &p)
	provider := providerOf(ctx, s, o)
	ref, cred, asUser, err := s.writeTarget(ctx, o.Repo, provider, p.Reviewer)
	if err != nil {
		s.finishWrite(ctx, o, "pending", nil, err, time.Now().Add(30*time.Second), true)
		return
	}
	if !asUser {
		// Only a person's own account may approve on the host; the verdict
		// still counts in Forge's gate.
		s.finishWrite(ctx, o, "done", map[string]string{"note": "reviewer has no linked account"}, nil, time.Time{}, true)
		return
	}
	if !s.budget(ctx, o, cred, ref.Provider) {
		return
	}
	cctx := s.callCtx(ctx, o.Class, cred)
	if o.ChangeID != nil && o.Attempts > 0 {
		if c, err := s.store.ChangeForAction(ctx, *o.ChangeID); err == nil {
			if src := vcs.ChangeSourceFor(provider); src != nil {
				prev, _ := s.store.StoredChange(ctx, o.Repo, c.Number)
				if ch, err := src.GetChange(cctx, s.client, ref, c.Number, prev); err == nil {
					_, _, _ = s.store.ApplyChange(ctx, o.Repo, provider, ch)
					login, _, _, _ := s.store.UserHostToken(ctx, p.Reviewer, provider)
					for _, rv := range ch.Reviews {
						if rv.Reviewer == login && rv.Verdict == p.Verdict && rv.CommitSHA == p.HeadSHA {
							s.finishWrite(ctx, o, "done", map[string]string{"note": "posted by an earlier attempt"}, nil, time.Time{}, true)
							return
						}
					}
				}
			}
		}
	}
	number := ""
	if o.ChangeID != nil {
		if c, err := s.store.ChangeForAction(ctx, *o.ChangeID); err == nil {
			number = c.Number
		}
	}
	id, err := w.PostReview(cctx, s.client, ref, number, p.Verdict, p.Body, p.HeadSHA)
	if err != nil {
		s.retryOrFail(ctx, o, err, cred, ref.Provider)
		return
	}
	s.finishWrite(ctx, o, "done", map[string]string{"review_id": id}, nil, time.Time{}, true)
}

// QueueComment queues a Forge comment for its host.
func QueueComment(ctx context.Context, st *store.Store, changeID int64, repo string, commentID int64, by string) error {
	payload, _ := json.Marshal(CommentPayload{CommentID: commentID})
	_, err := st.EnqueueOutbox(ctx, store.OutboxItem{IdemKey: "comment:" + strconv.FormatInt(commentID, 10),
		Kind: KindPostComment, Repo: repo, ChangeID: &changeID, Class: vcs.ClassUser, Payload: payload, RequestedBy: by}, "")
	return err
}

// QueueResolve queues telling the host a thread's resolved state. Toggles
// while one is queued collapse into one write of the latest state.
func QueueResolve(ctx context.Context, st *store.Store, changeID int64, repo string, threadID int64, by string) error {
	payload, _ := json.Marshal(ResolvePayload{ThreadID: threadID})
	_, err := st.EnqueueOutbox(ctx, store.OutboxItem{
		IdemKey: fmt.Sprintf("resolve:%d:%d", threadID, time.Now().UnixNano()),
		Kind:    KindResolveThread, Repo: repo, ChangeID: &changeID, Class: vcs.ClassUser, Payload: payload, RequestedBy: by,
	}, "")
	return err
}

// QueueReview queues a Forge verdict as the reviewer's own review on the
// host (sent only if they linked their account).
func QueueReview(ctx context.Context, st *store.Store, changeID int64, repo, reviewer, verdict, body, head string) error {
	payload, _ := json.Marshal(ReviewPayload{Verdict: verdict, Body: body, HeadSHA: head, Reviewer: reviewer})
	_, err := st.EnqueueOutbox(ctx, store.OutboxItem{
		IdemKey: fmt.Sprintf("review:%d:%s:%s:%s", changeID, reviewer, head, verdict),
		Kind:    KindPostReview, Repo: repo, ChangeID: &changeID, Class: vcs.ClassUser, Payload: payload, RequestedBy: reviewer,
	}, "")
	return err
}

// ---- re-anchoring sweep ----

// ReanchorThreads moves Forge-kept line threads of hosts without native
// threads to their lines on the change's new head (or marks them outdated),
// using the runner-computed diff between the two heads.
func (s *Service) ReanchorThreads(ctx context.Context) int {
	var providers []string
	for _, n := range vcs.Names() {
		if caps := vcs.CapabilitiesOf(n); !caps.Threads && caps.Diffs {
			providers = append(providers, n)
		}
	}
	threads, err := s.store.ThreadsToReanchor(ctx, providers, 50)
	if err != nil || len(threads) == 0 {
		return 0
	}
	moved := 0
	for _, a := range threads {
		k := store.DiffKey{Repo: a.Repo, From: a.AnchorHead, To: a.Head, Mode: "range"}
		d, err := s.store.GetDiff(ctx, k)
		if errors.Is(err, store.ErrNotFound) {
			refs, _ := vcs.MirrorRefsFor(a.Provider, a.Number, a.Source, a.Target)
			_, _ = s.store.RequestDiff(ctx, k, refs, false)
			continue
		}
		if err != nil || d.State == "pending" {
			continue
		}
		line := 0
		if d.State == "ready" && a.Side == "new" {
			patch, ok := s.filePatch(ctx, d, a.Path)
			if !ok {
				continue
			}
			line = Reanchor(patch, a.Line, a.Fingerprint)
		} else if a.Side == "old" {
			line = a.Line // the base did not move with the head
		}
		if err := s.store.Reanchor(ctx, a, line); err == nil {
			moved++
		}
	}
	return moved
}

// filePatch returns path's patch in a stored diff ("" when the file did not
// change); ok=false when the diff cannot be read now.
func (s *Service) filePatch(ctx context.Context, d *store.ChangeDiff, path string) (string, bool) {
	if s.blobs == nil {
		return "", false
	}
	rc, err := s.blobs.Get(ctx, d.BlobPrefix+"/manifest.json")
	if err != nil {
		return "", false
	}
	var m DiffManifest
	err = json.NewDecoder(rc).Decode(&m)
	rc.Close()
	if err != nil {
		return "", false
	}
	for _, f := range m.Files {
		if f.Path != path && f.OldPath != path {
			continue
		}
		if f.Status == "deleted" {
			return "deleted file mode\n", true
		}
		if f.Chunk == "" {
			return f.Patch, true
		}
		crc, err := s.blobs.Get(ctx, d.BlobPrefix+"/"+f.Chunk)
		if err != nil {
			return "", false
		}
		defer crc.Close()
		b, err := io.ReadAll(io.LimitReader(crc, 64<<20))
		return string(b), err == nil
	}
	return "", true
}
