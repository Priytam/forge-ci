package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/priytamjeepandey/forge-ci/internal/review"
	"github.com/priytamjeepandey/forge-ci/internal/store"
	"github.com/priytamjeepandey/forge-ci/internal/vcs"
)

// Code review writes (phase B): reviews and merges from Forge, and the merge
// rules. A write is saved at once; anything that must reach the host goes
// through the outbox, so these handlers never call a provider either.

func (s *Server) registerCodeReviewWriteRoutes() {
	s.mux.HandleFunc("POST /api/v1/changes/{id}/reviews", s.reviewChange)
	s.mux.HandleFunc("POST /api/v1/changes/{id}/merge", s.mergeChange)
	s.mux.HandleFunc("PUT /api/v1/merge-policies", s.putMergePolicy)
	s.mux.HandleFunc("DELETE /api/v1/merge-policies/{id}", s.deleteMergePolicy)
	s.mux.HandleFunc("PUT /api/v1/host-identities", s.putHostIdentity)
	s.mux.HandleFunc("DELETE /api/v1/host-identities/{provider}/{login}", s.deleteHostIdentity)
}

// defaultApproverRoles: who may approve when no rule says otherwise.
var defaultApproverRoles = []string{"admin", "owner", "developer"}

// changePolicy returns the merge rule that governs c (nil: none).
func (s *Server) changePolicy(ctx context.Context, c *store.ReviewTargetChange) (*store.MergePolicy, error) {
	ps, err := s.store.ListMergePolicies(ctx)
	if err != nil {
		return nil, err
	}
	return store.MatchMergePolicy(ps, c.Repo, c.TargetBranch), nil
}

// reviewerFor is the reviewing identity: the session's, or (SSO off, open
// bootstrap mode) the one the client names — the same rule job approvals use.
func (s *Server) reviewerFor(r *http.Request, claimed string) string {
	if actor := s.sessionActor(r); actor != "" {
		return strings.ToLower(actor)
	}
	if s.ssoEnforced(r.Context()) {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(claimed))
}

// mayReview reports whether email may give a deciding verdict on c, and why
// not.
func (s *Server) mayReview(ctx context.Context, c *store.ReviewTargetChange, p *store.MergePolicy, email string) (bool, string, error) {
	roles, group := defaultApproverRoles, ""
	if p != nil {
		roles, group = p.ApproverRoles, p.RequiredGroup
	}
	el, _, _, err := s.store.ApproverEligibility(ctx, c.Repo, []string{email}, roles, group)
	if err != nil {
		return false, "", err
	}
	if !el[email] {
		return false, "you need a " + strings.Join(roles, " / ") + " role on " + c.Repo + " to review here", nil
	}
	return true, "", nil
}

// reviewChange records a verdict given in Forge: POST
// /api/v1/changes/{id}/reviews {verdict: approve|request_changes|dismiss, body}.
// "dismiss" withdraws the caller's own verdict.
func (s *Server) reviewChange(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid change id")
		return
	}
	var req struct {
		Verdict  string `json:"verdict"`
		Body     string `json:"body"`
		Reviewer string `json:"reviewer"` // open bootstrap mode only
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	verdict := map[string]string{"approve": "approve", "request_changes": "request_changes", "dismiss": "dismissed"}[req.Verdict]
	if verdict == "" {
		writeErr(w, http.StatusBadRequest, "verdict must be approve, request_changes or dismiss")
		return
	}
	who := s.reviewerFor(r, req.Reviewer)
	if who == "" {
		writeErr(w, http.StatusBadRequest, "reviewer is required (sign in, or name one while SSO is off)")
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
	if c.State != "open" {
		writeErr(w, http.StatusConflict, "the change is "+c.State)
		return
	}
	p, err := s.changePolicy(r.Context(), c)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to load merge rules")
		return
	}
	if verdict != "dismissed" {
		ok, why, err := s.mayReview(r.Context(), c, p, who)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "failed to check approver role")
			return
		}
		if !ok {
			s.audit(r, "change.review.denied", c.Number, c.Repo, "denied", map[string]any{"verdict": verdict})
			writeErr(w, http.StatusForbidden, why)
			return
		}
		if verdict == "approve" && (p == nil || !p.AllowSelfApproval) {
			author, err := s.store.HostIdentityEmail(r.Context(), c.Provider, c.Author)
			if err != nil {
				writeErr(w, http.StatusInternalServerError, "failed to check the author")
				return
			}
			if author == who {
				writeErr(w, http.StatusForbidden, "you may not approve your own change")
				return
			}
		}
	}
	if err := s.store.SetForgeReview(r.Context(), id, who, verdict, strings.TrimSpace(req.Body), c.HeadSHA); err != nil {
		slog.Error("code review: save review", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to save review")
		return
	}
	// A reviewer who linked their host account has the verdict posted as
	// their own review there too (only they can approve on the host).
	if (verdict == "approve" || verdict == "request_changes") && vcs.ThreadWriterFor(c.Provider) != nil {
		if _, _, linked, err := s.store.UserHostToken(r.Context(), who, c.Provider); err == nil && linked {
			if err := review.QueueReview(r.Context(), s.store, c.ID, c.Repo, who, verdict, strings.TrimSpace(req.Body), c.HeadSHA); err != nil {
				slog.Error("code review: queue host review", "err", err)
			}
		}
	}
	s.audit(r, "change.review", c.Repo+"#"+c.Number, c.Repo, "ok", map[string]any{"verdict": verdict, "head": c.HeadSHA})
	writeJSON(w, http.StatusOK, map[string]any{"reviewer": who, "verdict": verdict, "head_sha": c.HeadSHA})
}

// mergeChange queues a merge: POST /api/v1/changes/{id}/merge {method,
// expected_head}. It is accepted only for the head the caller is looking at
// and while the gate passes; the outbox re-checks both before the host is
// called. Answers 202 with the queued action.
func (s *Server) mergeChange(w http.ResponseWriter, r *http.Request) {
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
	caps := vcs.CapabilitiesOf(c.Provider)
	if len(caps.MergeMethods) == 0 {
		writeErr(w, http.StatusConflict, "changes on this host cannot be merged from Forge yet")
		return
	}
	if req.Method == "" {
		req.Method = caps.MergeMethods[0]
	}
	if !slices.Contains(caps.MergeMethods, req.Method) {
		writeErr(w, http.StatusBadRequest, "method must be one of "+strings.Join(caps.MergeMethods, ", "))
		return
	}
	p, err := s.changePolicy(r.Context(), c)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to load merge rules")
		return
	}
	if p != nil && p.MergeQueue {
		writeErr(w, http.StatusConflict, c.TargetBranch+" merges through the merge queue — add the change to the queue")
		return
	}
	if p != nil {
		g, err := s.store.GetGate(r.Context(), id)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "failed to load the merge gate")
			return
		}
		if g == nil {
			writeErr(w, http.StatusConflict, "the merge gate has not been evaluated yet — try again in a moment")
			return
		}
		if g.State != "pass" {
			writeErr(w, http.StatusConflict, "the merge gate does not pass: "+g.Summary)
			return
		}
	}
	payload, _ := json.Marshal(review.MergePayload{Method: req.Method, ExpectedHead: c.HeadSHA})
	actor := s.sessionActor(r)
	o, err := s.store.EnqueueOutbox(r.Context(), store.OutboxItem{
		IdemKey: fmt.Sprintf("merge:%d:%s", id, c.HeadSHA), Kind: review.KindMerge, Repo: c.Repo,
		ChangeID: &id, Class: vcs.ClassUser, Payload: payload, RequestedBy: actor,
	}, "")
	if err != nil {
		slog.Error("code review: queue merge", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to queue the merge")
		return
	}
	s.audit(r, "change.merge", c.Repo+"#"+c.Number, c.Repo, "ok", map[string]any{"method": req.Method, "head": c.HeadSHA})
	writeJSON(w, http.StatusAccepted, map[string]any{"action": o})
}

// ---- merge rules ----

func (s *Server) listMergePolicies(w http.ResponseWriter, r *http.Request) {
	ps, err := s.store.ListMergePolicies(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to list merge rules")
		return
	}
	writeJSONCached(w, r, ps)
}

// putMergePolicy creates or replaces the rule for (repo, branch_pattern).
// Omitted booleans take the safe defaults (CI required, approvals dismissed
// on push, changes requested block).
func (s *Server) putMergePolicy(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	p := store.MergePolicy{RequiredApprovals: 1, RequireCI: true, DismissOnPush: true, BlockOnChangesRequested: true}
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	p.UpdatedBy = s.sessionActor(r)
	out, err := s.store.UpsertMergePolicy(r.Context(), p)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.audit(r, "merge-policy.put", out.Repo+":"+out.BranchPattern, out.Repo, "ok", map[string]any{
		"required_approvals": out.RequiredApprovals, "required_group": out.RequiredGroup, "require_ci": out.RequireCI})
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) deleteMergePolicy(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid id")
		return
	}
	p, err := s.store.DeleteMergePolicy(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "merge rule not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to delete merge rule")
		return
	}
	s.audit(r, "merge-policy.delete", p.Repo+":"+p.BranchPattern, p.Repo, "ok", nil)
	w.WriteHeader(http.StatusNoContent)
}

// ---- host identities ----

func (s *Server) listHostIdentities(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	ids, err := s.store.ListHostIdentities(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to list identities")
		return
	}
	writeJSONCached(w, r, ids)
}

func (s *Server) putHostIdentity(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var h store.HostIdentity
	if err := json.NewDecoder(r.Body).Decode(&h); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if !vcs.Known(h.Provider) {
		writeErr(w, http.StatusBadRequest, "provider must be one of: "+strings.Join(vcs.Names(), ", "))
		return
	}
	if err := s.store.PutHostIdentity(r.Context(), h); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.audit(r, "host-identity.put", h.Provider+":"+h.Login, "", "ok", map[string]any{"email": h.Email})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteHostIdentity(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	provider, login := r.PathValue("provider"), r.PathValue("login")
	err := s.store.DeleteHostIdentity(r.Context(), provider, login)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "identity not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to delete identity")
		return
	}
	s.audit(r, "host-identity.delete", provider+":"+login, "", "ok", nil)
	w.WriteHeader(http.StatusNoContent)
}
