package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/priytamjeepandey/forge-ci/internal/proto"
	"github.com/priytamjeepandey/forge-ci/internal/store"
)

// Template catalog: org-wide, versioned pipeline templates. Anyone signed in
// can browse them (they're meant to be included); admins publish and remove
// versions. Versions are immutable — a fix is a new version, so a pipeline
// pinned to deploy/k8s@1.4.0 always builds the same way.

func (s *Server) registerCatalogRoutes() {
	m := s.mux
	m.HandleFunc("GET /api/v1/template-catalog", s.listCatalog)
	m.HandleFunc("POST /api/v1/template-catalog", s.publishCatalogTemplate)
	m.HandleFunc("DELETE /api/v1/template-catalog", s.deleteCatalogTemplate)
}

// listCatalog lists every version, or one when ?name= (and optional
// ?version=, default newest) is given.
func (s *Server) listCatalog(w http.ResponseWriter, r *http.Request) {
	if name := r.URL.Query().Get("name"); name != "" {
		t, err := s.store.GetCatalogTemplate(r.Context(), name, r.URL.Query().Get("version"))
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "template not found")
			return
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "failed to load template")
			return
		}
		writeJSON(w, http.StatusOK, t)
		return
	}
	all, err := s.store.ListCatalog(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to list templates")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"templates": all})
}

func (s *Server) publishCatalogTemplate(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var req proto.CatalogTemplate
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req.PublishedBy = s.actorLabel(r)
	if err := s.store.PublishCatalogTemplate(r.Context(), req); err != nil {
		if errors.Is(err, store.ErrVersionExists) {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.audit(r, "template-catalog.publish", req.Name+"@"+req.Version, "", "ok", map[string]any{"name": req.Name, "version": req.Version})
	t, _ := s.store.GetCatalogTemplate(r.Context(), req.Name, req.Version)
	writeJSON(w, http.StatusCreated, t)
}

func (s *Server) deleteCatalogTemplate(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	name, version := r.URL.Query().Get("name"), r.URL.Query().Get("version")
	if name == "" || version == "" {
		writeErr(w, http.StatusBadRequest, "name and version are required")
		return
	}
	if err := s.store.DeleteCatalogTemplate(r.Context(), name, version); errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "template version not found")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to delete")
		return
	}
	s.audit(r, "template-catalog.delete", name+"@"+version, "", "ok", nil)
	w.WriteHeader(http.StatusNoContent)
}
