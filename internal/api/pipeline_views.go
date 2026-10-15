package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/priytamjeepandey/forge-ci/internal/store"
)

// Saved pipeline-list views: a user names a set of filters (usually several
// repos plus status/runner/environment) and returns to it later. Views are
// stored per user on the server; a shared view is visible to everyone but
// only its owner (or an admin) may change it.

func (s *Server) registerPipelineViewRoutes() {
	m := s.mux
	m.HandleFunc("GET /api/v1/pipeline-facets", s.pipelineFacets)
	m.HandleFunc("GET /api/v1/pipeline-views", s.listPipelineViews)
	m.HandleFunc("POST /api/v1/pipeline-views", s.createPipelineView)
	m.HandleFunc("PUT /api/v1/pipeline-views/{id}", s.updatePipelineView)
	m.HandleFunc("DELETE /api/v1/pipeline-views/{id}", s.deletePipelineView)
}

// queryList reads a repeatable, optionally comma-separated query parameter.
func queryList(r *http.Request, key string) []string {
	var out []string
	for _, v := range r.URL.Query()[key] {
		for _, part := range strings.Split(v, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}

func pipelineFilterFromQuery(r *http.Request) store.PipelineFilter {
	q := r.URL.Query()
	return store.PipelineFilter{
		Repos:       queryList(r, "repo"),
		Ref:         strings.TrimSpace(q.Get("ref")),
		Statuses:    queryList(r, "status"),
		Runner:      strings.TrimSpace(q.Get("runner")),
		Environment: strings.TrimSpace(q.Get("environment")),
	}
}

// pipelineFacets returns the values the filter dropdowns offer, optionally
// narrowed to ?repo.
func (s *Server) pipelineFacets(w http.ResponseWriter, r *http.Request) {
	f, err := s.store.ListPipelineFacets(r.Context(), queryList(r, "repo"))
	if err != nil {
		slog.Error("pipeline facets", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to load filter values")
		return
	}
	writeJSON(w, http.StatusOK, f)
}

type pipelineViewRequest struct {
	Name    string               `json:"name"`
	Filters store.PipelineFilter `json:"filters"`
	Shared  bool                 `json:"shared"`
}

func decodeViewRequest(r *http.Request) (*pipelineViewRequest, error) {
	var req pipelineViewRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return nil, errors.New("invalid JSON body")
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len(req.Name) > 100 {
		return nil, errors.New("name is required (max 100 characters)")
	}
	if err := req.Filters.Validate(); err != nil {
		return nil, err
	}
	return &req, nil
}

func (s *Server) listPipelineViews(w http.ResponseWriter, r *http.Request) {
	views, err := s.store.ListPipelineViews(r.Context(), s.sessionActor(r))
	if err != nil {
		slog.Error("list pipeline views", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to list views")
		return
	}
	writeJSON(w, http.StatusOK, views)
}

func (s *Server) createPipelineView(w http.ResponseWriter, r *http.Request) {
	req, err := decodeViewRequest(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	v, err := s.store.CreatePipelineView(r.Context(), s.sessionActor(r), req.Name, req.Filters, req.Shared)
	if errors.Is(err, store.ErrDuplicateView) {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		slog.Error("create pipeline view", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to save view")
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

// ownedView loads view {id} and checks the caller may change it: its owner,
// or an admin. Writes the error response and returns nil otherwise.
func (s *Server) ownedView(w http.ResponseWriter, r *http.Request) *store.PipelineView {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid view id")
		return nil
	}
	v, err := s.store.GetPipelineView(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "view not found")
		return nil
	}
	if err != nil {
		slog.Error("get pipeline view", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to load view")
		return nil
	}
	if !strings.EqualFold(v.Owner, s.sessionActor(r)) && !s.isAdmin(r) {
		if !v.Shared {
			// Someone else's private view: don't reveal it exists.
			writeErr(w, http.StatusNotFound, "view not found")
			return nil
		}
		writeErr(w, http.StatusForbidden, "only the view's owner can change it")
		return nil
	}
	return v
}

func (s *Server) updatePipelineView(w http.ResponseWriter, r *http.Request) {
	v := s.ownedView(w, r)
	if v == nil {
		return
	}
	req, err := decodeViewRequest(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	updated, err := s.store.UpdatePipelineView(r.Context(), v.ID, req.Name, req.Filters, req.Shared)
	if errors.Is(err, store.ErrDuplicateView) {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		slog.Error("update pipeline view", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to save view")
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) deletePipelineView(w http.ResponseWriter, r *http.Request) {
	v := s.ownedView(w, r)
	if v == nil {
		return
	}
	if err := s.store.DeletePipelineView(r.Context(), v.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		slog.Error("delete pipeline view", "err", err, "id", strconv.FormatInt(v.ID, 10))
		writeErr(w, http.StatusInternalServerError, "failed to delete view")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
