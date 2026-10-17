package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/autoscale"
	"github.com/priytamjeepandey/forge-ci/internal/store"
)

// Runner pools (admin): autoscaled sets of runners.

func (s *Server) registerRunnerPoolRoutes() {
	m := s.mux
	m.HandleFunc("GET /api/v1/runner-pools", s.listRunnerPools)
	m.HandleFunc("POST /api/v1/runner-pools", s.saveRunnerPool)
	m.HandleFunc("PUT /api/v1/runner-pools/{id}", s.saveRunnerPool)
	m.HandleFunc("DELETE /api/v1/runner-pools/{id}", s.deleteRunnerPool)
	m.HandleFunc("POST /api/v1/runner-pools/evaluate", s.evaluateRunnerPools)
}

const masked = "••••••••"

// maskSettings hides the provider's secret keys in a settings JSON.
func maskSettings(provider, raw string) string {
	st, err := autoscale.ParseSettings(raw)
	if err != nil {
		return ""
	}
	if p, ok := autoscale.Lookup(provider); ok {
		if secrets, err := p.Validate(st); err == nil {
			for _, k := range secrets {
				if st[k] != "" {
					st[k] = masked
				}
			}
		}
	}
	b, _ := json.Marshal(st)
	return string(b)
}

func (s *Server) listRunnerPools(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	pools, err := s.store.ListRunnerPools(r.Context(), true)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to list pools")
		return
	}
	for i := range pools {
		pools[i].Settings = maskSettings(pools[i].Provider, pools[i].Settings)
	}
	writeJSON(w, http.StatusOK, map[string]any{"pools": pools, "providers": autoscale.Names()})
}

func (s *Server) saveRunnerPool(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var in store.RunnerPool
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	in.ID = 0
	var existing *store.RunnerPool
	if r.Method == http.MethodPut {
		id, ok := pathID(r)
		if !ok {
			writeErr(w, http.StatusBadRequest, "invalid id")
			return
		}
		in.ID = id
		if e, err := s.store.GetRunnerPool(r.Context(), id); err == nil {
			existing = e
		}
	}
	prov, ok := autoscale.Lookup(in.Provider)
	if !ok {
		writeErr(w, http.StatusBadRequest, "provider must be one of "+strings.Join(autoscale.Names(), ", "))
		return
	}
	settings, err := autoscale.ParseSettings(in.Settings)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// Masked secret values mean "keep what is stored".
	if existing != nil {
		old, _ := autoscale.ParseSettings(existing.Settings)
		for k, v := range settings {
			if v == masked {
				settings[k] = old[k]
			}
		}
	}
	if _, err := prov.Validate(settings); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	raw, _ := json.Marshal(settings)
	in.Settings = string(raw)
	out, err := s.store.SaveRunnerPool(r.Context(), in)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, "pool not found")
		return
	case errors.Is(err, store.ErrDuplicatePool):
		writeErr(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		if strings.Contains(err.Error(), "must") {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		slog.Error("save runner pool", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to save pool")
		return
	}
	s.audit(r, "runner-pool.save", out.Name, "", "ok", map[string]any{
		"id": out.ID, "provider": out.Provider, "tags": out.Tags, "min": out.Min, "max": out.Max, "enabled": out.Enabled,
	})
	out.Settings = maskSettings(out.Provider, out.Settings)
	code := http.StatusOK
	if r.Method == http.MethodPost {
		code = http.StatusCreated
	}
	writeJSON(w, code, out)
}

func (s *Server) deleteRunnerPool(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid id")
		return
	}
	if err := s.store.DeleteRunnerPool(r.Context(), id); errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "pool not found")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to delete pool")
		return
	}
	s.audit(r, "runner-pool.delete", strconv.FormatInt(id, 10), "", "ok", nil)
	w.WriteHeader(http.StatusNoContent)
}

// evaluateRunnerPools runs one autoscale pass now (the loop runs every 30s).
func (s *Server) evaluateRunnerPools(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if err := autoscale.Pass(r.Context(), s.store, time.Now()); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	s.listRunnerPools(w, r)
}
