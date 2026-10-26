package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/review"
	"github.com/priytamjeepandey/forge-ci/internal/store"
	"github.com/priytamjeepandey/forge-ci/internal/vcs"
)

// Code review phase F: Forge-native changes (opened, edited and closed in
// Forge on hosts with no pull requests) and the merge queue. Like every
// code review write, these save to Postgres and return; runners and the
// review worker do the rest.

func (s *Server) registerNativeChangeRoutes() {
	s.mux.HandleFunc("POST /api/v1/changes", s.createChange)
	s.mux.HandleFunc("PATCH /api/v1/changes/{id}", s.editChange)
	s.mux.HandleFunc("POST /api/v1/changes/{id}/queue", s.enqueueChange)
	s.mux.HandleFunc("DELETE /api/v1/changes/{id}/queue", s.dequeueChange)
}

// debounceNative coalesces a burst of pushes into one refresh.
const debounceNative = 2 * time.Second

var branchNameRe = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,200}$`)

func validBranchName(b string) bool {
	return branchNameRe.MatchString(b) && !strings.HasPrefix(b, "-") && !strings.HasPrefix(b, "/") &&
		!strings.HasSuffix(b, "/") && !strings.Contains(b, "..") && !strings.HasSuffix(b, ".lock") &&
		!strings.HasPrefix(b, review.QueueBranchPrefix)
}

const maxChangeBody = 64 << 10

// createChange opens a Forge-native change: POST /api/v1/changes
// {repo, source_branch, target_branch, title, body, draft}.
func (s *Server) createChange(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Repo         string `json:"repo"`
		SourceBranch string `json:"source_branch"`
		TargetBranch string `json:"target_branch"`
		Title        string `json:"title"`
		Body         string `json:"body"`
		Draft        bool   `json:"draft"`
		Author       string `json:"author"` // open bootstrap mode only
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxChangeBody+4096)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req.Title = strings.TrimSpace(req.Title)
	switch {
	case req.Repo == "" || req.Title == "":
		writeErr(w, http.StatusBadRequest, "repo and title are required")
		return
	case len(req.Title) > 300 || len(req.Body) > maxChangeBody:
		writeErr(w, http.StatusBadRequest, "title or description too long")
		return
	case !validBranchName(req.SourceBranch) || !validBranchName(req.TargetBranch):
		writeErr(w, http.StatusBadRequest, "source_branch and target_branch must be branch names")
		return
	case req.SourceBranch == req.TargetBranch:
		writeErr(w, http.StatusBadRequest, "a change needs two different branches")
		return
	}
	provider, found, err := s.store.RegisteredProvider(r.Context(), req.Repo)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to load the repo")
		return
	}
	if !found {
		writeErr(w, http.StatusNotFound, "repo not connected")
		return
	}
	if !vcs.CapabilitiesOf(provider).NativeChanges {
		writeErr(w, http.StatusConflict, "changes on "+provider+" are opened on the host; Forge shows them by itself")
		return
	}
	if !s.requireOperate(w, r, req.Repo) {
		return
	}
	author := s.reviewerFor(r, req.Author)
	if author == "" {
		author = "anonymous" // open bootstrap mode, nobody named (SSO on always has a session here)
	}
	id, number, err := s.store.CreateNativeChange(r.Context(), store.NativeChange{Repo: req.Repo, Provider: provider,
		Author: author, Title: req.Title, Body: req.Body, SourceBranch: req.SourceBranch, TargetBranch: req.TargetBranch,
		Draft: req.Draft})
	if errors.Is(err, store.ErrDuplicateChange) {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		slog.Error("code review: create change", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to create the change")
		return
	}
	// The first refresh reads the two branch tips (and closes the change if
	// the source branch does not exist).
	_ = s.store.RequestChangeRefresh(r.Context(), req.Repo, provider, number, store.PriorityWebhook, 0, false)
	s.audit(r, "change.create", req.Repo+"#"+number, req.Repo, "ok",
		map[string]any{"source": req.SourceBranch, "target": req.TargetBranch})
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "number": number})
}

// editChange edits a native change: PATCH /api/v1/changes/{id}
// {title?, body?, draft?, state?: open|closed}.
func (s *Server) editChange(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid change id")
		return
	}
	var req store.NativeEdit
	var body struct {
		Title *string `json:"title"`
		Body  *string `json:"body"`
		Draft *bool   `json:"draft"`
		State *string `json:"state"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxChangeBody+4096)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req = store.NativeEdit(body)
	if req.Title != nil {
		t := strings.TrimSpace(*req.Title)
		if t == "" || len(t) > 300 {
			writeErr(w, http.StatusBadRequest, "title must be 1–300 characters")
			return
		}
		req.Title = &t
	}
	if (req.Body != nil && len(*req.Body) > maxChangeBody) || (req.State != nil && *req.State != "open" && *req.State != "closed") {
		writeErr(w, http.StatusBadRequest, "state must be open or closed; description at most 64 KB")
		return
	}
	c, err := s.store.ChangeForAction(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "change not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to load change")
		return
	}
	if !vcs.CapabilitiesOf(c.Provider).NativeChanges {
		writeErr(w, http.StatusConflict, "this change lives on "+c.Provider+": edit it there")
		return
	}
	if !s.requireOperate(w, r, c.Repo) {
		return
	}
	if err := s.store.EditNativeChange(r.Context(), id, req); errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusConflict, "a merged change cannot be edited")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to save the change")
		return
	}
	if req.State != nil && *req.State == "open" {
		_ = s.store.RequestChangeRefresh(r.Context(), c.Repo, c.Provider, c.Number, store.PriorityWebhook, 0, false)
	}
	s.audit(r, "change.edit", c.Repo+"#"+c.Number, c.Repo, "ok", nil)
	w.WriteHeader(http.StatusNoContent)
}

// queueView is the change page's merge queue panel.
type queueView struct {
	Enabled bool              `json:"enabled"`
	Entry   *store.QueueEntry `json:"entry,omitempty"` // live, else the latest finished one on this head
	Length  int               `json:"length"`
}

func (s *Server) changeQueue(ctx context.Context, d *store.ChangeDetail, p *store.MergePolicy) (*queueView, error) {
	v := &queueView{Enabled: p != nil && p.MergeQueue}
	e, err := s.store.ActiveQueueEntry(ctx, d.ID)
	if err != nil {
		return nil, err
	}
	if e == nil {
		if e, err = s.store.LatestQueueEntry(ctx, d.ID); err != nil {
			return nil, err
		}
		if e != nil && e.HeadSHA != d.HeadSHA {
			e = nil
		}
	}
	v.Entry = e
	if v.Enabled {
		list, err := s.store.ListQueue(ctx, d.Repo, d.TargetBranch)
		if err != nil {
			return nil, err
		}
		v.Length = len(list)
	}
	return v, nil
}

// enqueueChange adds a change to its target's merge queue: POST
// /api/v1/changes/{id}/queue {method, expected_head}.
func (s *Server) enqueueChange(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid change id")
		return
	}
	var req struct {
		Method       string `json:"method"`
		ExpectedHead string `json:"expected_head"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ExpectedHead == "" {
		writeErr(w, http.StatusBadRequest, "expected_head is required")
		return
	}
	c, err := s.store.ChangeForAction(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "change not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to load change")
		return
	}
	if !s.requireOperate(w, r, c.Repo) {
		return
	}
	switch {
	case c.State != "open":
		writeErr(w, http.StatusConflict, "the change is "+c.State)
		return
	case c.Draft:
		writeErr(w, http.StatusConflict, "the change is a draft")
		return
	case c.HeadSHA != req.ExpectedHead:
		writeErr(w, http.StatusConflict, "the change has new commits since you loaded it — reload and review them")
		return
	}
	p, err := s.changePolicy(r.Context(), c)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to load merge rules")
		return
	}
	if p == nil || !p.MergeQueue {
		writeErr(w, http.StatusConflict, review.ErrQueueOff.Error())
		return
	}
	caps := vcs.CapabilitiesOf(c.Provider)
	if req.Method == "" && len(caps.MergeMethods) > 0 {
		req.Method = caps.MergeMethods[0]
	}
	if !slices.Contains(caps.MergeMethods, req.Method) {
		writeErr(w, http.StatusBadRequest, "method must be one of "+strings.Join(caps.MergeMethods, ", "))
		return
	}
	g, err := s.store.GetGate(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to load the merge gate")
		return
	}
	if g == nil || g.HeadSHA != c.HeadSHA {
		writeErr(w, http.StatusConflict, "the merge gate has not been evaluated yet — try again in a moment")
		return
	}
	for _, rule := range g.Rules {
		if rule.ID != "merge_queue" && rule.ID != "freeze" && rule.State != "pass" {
			writeErr(w, http.StatusConflict, "the merge gate does not pass: "+rule.Detail)
			return
		}
	}
	e, err := s.store.EnqueueMerge(r.Context(), store.QueueEntry{ChangeID: id, Repo: c.Repo, TargetBranch: c.TargetBranch,
		HeadSHA: c.HeadSHA, Method: req.Method, RequestedBy: s.sessionActor(r)})
	if errors.Is(err, store.ErrAlreadyQueued) {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		slog.Error("merge queue: enqueue", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to add the change to the queue")
		return
	}
	s.audit(r, "change.queue", c.Repo+"#"+c.Number, c.Repo, "ok", map[string]any{"method": req.Method, "head": c.HeadSHA})
	writeJSON(w, http.StatusAccepted, map[string]any{"entry": e})
}

// dequeueChange takes a change out of its queue: DELETE /api/v1/changes/{id}/queue.
func (s *Server) dequeueChange(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid change id")
		return
	}
	c, err := s.store.ChangeForAction(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "change not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to load change")
		return
	}
	if !s.requireOperate(w, r, c.Repo) {
		return
	}
	who := s.sessionActor(r)
	if who == "" {
		who = "someone"
	}
	removed, err := s.store.RemoveFromQueue(r.Context(), id, "removed by "+who)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to remove the change from the queue")
		return
	}
	if !removed {
		writeErr(w, http.StatusNotFound, "the change is not in a merge queue")
		return
	}
	s.audit(r, "change.dequeue", c.Repo+"#"+c.Number, c.Repo, "ok", nil)
	w.WriteHeader(http.StatusNoContent)
}

// listMergeQueue: GET /api/v1/merge-queue[?repo=&target=] — live queues in
// order.
func (s *Server) listMergeQueue(w http.ResponseWriter, r *http.Request) {
	repo, target := r.URL.Query().Get("repo"), r.URL.Query().Get("target")
	list, err := s.store.ListQueue(r.Context(), repo, target)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to load the merge queue")
		return
	}
	writeJSONCached(w, r, list)
}

// nudgeNativeChanges asks for a refresh of the native changes a push to
// repo's ref moved (a plain git server sends no change events, but the
// push that starts a pipeline says a branch moved).
func (s *Server) nudgeNativeChanges(ctx context.Context, repo, ref string) {
	provider, found, err := s.store.RegisteredProvider(ctx, repo)
	if err != nil || !found || !vcs.CapabilitiesOf(provider).NativeChanges {
		return
	}
	numbers, err := s.store.NativeChangesOn(ctx, repo, strings.TrimPrefix(ref, "refs/heads/"))
	if err != nil {
		slog.Warn("code review: native changes on push", "repo", repo, "err", err)
		return
	}
	for _, n := range numbers {
		if err := s.store.RequestChangeRefresh(ctx, repo, provider, n, store.PriorityWebhook, debounceNative, true); err != nil {
			slog.Warn("code review: request refresh", "repo", repo, "change", n, "err", fmt.Sprint(err))
		}
	}
}
