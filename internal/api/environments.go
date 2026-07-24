package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/priytamjeepandey/forge-ci/internal/compiler"
	"github.com/priytamjeepandey/forge-ci/internal/proto"
	"github.com/priytamjeepandey/forge-ci/internal/store"
)

// registerEnvironmentRoutes wires the ArgoCD-style environments board.
//
// Repo names contain slashes ("owner/repo"), which a single {repo} path
// wildcard cannot capture. The sub-resources are therefore served from a subtree
// handler ("/api/v1/environments/") that parses the trailing "<repo…>/<env>/<action>"
// itself; the board root is an exact match. Freeze windows get their own routes.
func (s *Server) registerEnvironmentRoutes() {
	m := s.mux
	m.HandleFunc("GET /api/v1/environments", s.listEnvironments)
	m.HandleFunc("GET /api/v1/environments/", s.getEnvironmentSubtree)
	m.HandleFunc("POST /api/v1/environments/", s.postEnvironmentSubtree)

	m.HandleFunc("GET /api/v1/deploy-freezes", s.listFreezes)
	m.HandleFunc("POST /api/v1/deploy-freezes", s.createFreeze)
	m.HandleFunc("DELETE /api/v1/deploy-freezes/{id}", s.deleteFreeze)
}

// parseEnvPath extracts (repo, env) from ".../environments/<repo…>/<env>/<action>".
// repo may contain slashes; env is the single segment before the action.
func parseEnvPath(path, action string) (repo, env string, ok bool) {
	rest := strings.TrimPrefix(path, "/api/v1/environments/")
	rest = strings.TrimSuffix(rest, "/")
	if !strings.HasSuffix(rest, "/"+action) {
		return "", "", false
	}
	rest = strings.TrimSuffix(rest, "/"+action)
	i := strings.LastIndex(rest, "/")
	if i < 0 {
		return "", "", false
	}
	repo, env = rest[:i], rest[i+1:]
	if repo == "" || env == "" {
		return "", "", false
	}
	return repo, env, true
}

// driftOf compares a deployed sha against the resolved ref tip. An empty tipSHA
// means the tip could not be resolved (repo not connected) → drift unknown.
func driftOf(deployedSHA, tipSHA string) string {
	switch {
	case tipSHA == "":
		return proto.DriftUnknown
	case tipSHA == deployedSHA:
		return proto.DriftInSync
	default:
		return proto.DriftDrifted
	}
}

// listEnvironments returns the board for one repo: one entry per environment with
// its current (latest successful) deployment, deployment count, drift indicator
// and active-freeze flag.
func (s *Server) listEnvironments(w http.ResponseWriter, r *http.Request) {
	repo := r.URL.Query().Get("repo")
	if repo == "" {
		writeErr(w, http.StatusBadRequest, "repo query parameter is required")
		return
	}
	rows, err := s.store.EnvironmentsForRepo(r.Context(), repo)
	if err != nil {
		slog.Error("list environments", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to list environments")
		return
	}
	out := make([]proto.EnvironmentBoard, 0, len(rows))
	tipCache := map[string]string{} // ref -> tip sha (avoid re-resolving a shared ref)
	for i := range rows {
		row := rows[i]
		cur := row.Current
		card := proto.EnvironmentBoard{
			Repo:            repo,
			Environment:     row.Environment,
			Current:         &cur,
			DeploymentCount: row.Count,
			Drift:           proto.DriftUnknown,
		}
		// Drift: compare the deployed sha against the current tip of its ref. For
		// unconnected repos (ResolveRef → ErrNotFound) drift stays "unknown".
		tip, cached := tipCache[cur.Ref]
		if !cached {
			resolved, rerr := s.store.ResolveRef(r.Context(), repo, cur.Ref)
			if rerr == nil {
				tip = resolved
				tipCache[cur.Ref] = resolved
			} else if !errors.Is(rerr, store.ErrNotFound) {
				slog.Warn("environments: resolve ref tip", "repo", repo, "ref", cur.Ref, "err", rerr)
			}
		}
		card.RefTipSHA = tip
		card.Drift = driftOf(cur.SHA, tip)
		if frozen, ferr := s.store.IsFrozen(r.Context(), repo, row.Environment); ferr == nil {
			card.Frozen = frozen
		}
		out = append(out, card)
	}
	writeJSON(w, http.StatusOK, out)
}

// getEnvironmentSubtree serves GET .../environments/<repo>/<env>/deployments.
func (s *Server) getEnvironmentSubtree(w http.ResponseWriter, r *http.Request) {
	repo, env, ok := parseEnvPath(r.URL.Path, "deployments")
	if !ok {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	limit, offset := pageParams(r)
	deps, total, err := s.store.ListDeployments(r.Context(), repo, env, limit, offset)
	if err != nil {
		slog.Error("list deployments", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to list deployments")
		return
	}
	w.Header().Set("X-Total-Count", strconv.Itoa(total))
	w.Header().Set("X-Has-More", strconv.FormatBool(offset+len(deps) < total))
	writeJSON(w, http.StatusOK, deps)
}

// postEnvironmentSubtree serves POST .../environments/<repo>/<env>/rollback.
func (s *Server) postEnvironmentSubtree(w http.ResponseWriter, r *http.Request) {
	repo, env, ok := parseEnvPath(r.URL.Path, "rollback")
	if !ok {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	s.rollback(w, r, repo, env)
}

// rollback re-deploys a prior deployment by creating a NEW pipeline at the
// target's SHA/ref from the repo's registered config. The rollback goes through
// the normal pipeline flow — including the environment's approval gate — so a
// rollback to a protected environment is BLOCKED awaiting approval, never a
// bypass. Admin-gated and audited.
func (s *Server) rollback(w http.ResponseWriter, r *http.Request, repo, env string) {
	if !s.requireAdmin(w, r) {
		return
	}
	var req proto.RollbackRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.ToPipelineID <= 0 && req.ToSHA == "" {
		writeErr(w, http.StatusBadRequest, "one of to_pipeline_id or to_sha is required")
		return
	}

	targetSHA, targetRef, err := s.store.RollbackTarget(r.Context(), repo, env, req.ToPipelineID, req.ToSHA)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "no prior deployment of that target to this environment")
		return
	}
	if err != nil {
		slog.Error("rollback target", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to resolve rollback target")
		return
	}

	config, err := s.store.GetRepoConfig(r.Context(), repo)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusBadRequest, "repo has no registered config to roll back with (register one first)")
		return
	}
	if err != nil {
		slog.Error("rollback get config", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to load repo config")
		return
	}

	jobs, err := compiler.Compile(config, targetRef, compiler.SourceAPI, s.store.TemplateResolver(r.Context(), repo))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "registered config no longer compiles: "+err.Error())
		return
	}
	opts, err := compiler.Options(config)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	// Stamp the config version when the registered config is the current one.
	var configVersion *int
	if v, verr := s.store.CurrentConfigVersion(r.Context(), repo); verr == nil && v > 0 {
		configVersion = &v
	}

	createReq := proto.CreatePipelineRequest{
		Repo:        repo,
		Ref:         targetRef,
		SHA:         targetSHA,
		Config:      config,
		TriggeredBy: s.sessionActor(r), // "" in open bootstrap mode
	}
	p, err := s.store.CreatePipeline(r.Context(), createReq, jobs, configVersion, opts.AutoCancel)
	if err != nil {
		slog.Error("rollback create pipeline", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to create rollback pipeline")
		return
	}

	selector := "sha:" + targetSHA
	if req.ToPipelineID > 0 {
		selector = "pipeline:" + strconv.FormatInt(req.ToPipelineID, 10)
	}
	s.audit(r, "environment.rollback", strconv.FormatInt(p.ID, 10), repo, "ok", map[string]any{
		"environment":     env,
		"target":          selector,
		"target_sha":      targetSHA,
		"target_ref":      targetRef,
		"new_pipeline_id": p.ID,
	})
	slog.Info("environment rollback", "repo", repo, "env", env, "target_sha", targetSHA, "pipeline", p.ID)
	writeJSON(w, http.StatusCreated, map[string]any{"pipeline": p})
}

// ---- deploy freezes ----

func (s *Server) listFreezes(w http.ResponseWriter, r *http.Request) {
	freezes, err := s.store.ListFreezes(r.Context(), r.URL.Query().Get("repo"))
	if err != nil {
		slog.Error("list freezes", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to list deploy freezes")
		return
	}
	writeJSON(w, http.StatusOK, freezes)
}

func (s *Server) createFreeze(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var f proto.DeployFreeze
	if err := json.NewDecoder(r.Body).Decode(&f); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if f.Environment == "" {
		writeErr(w, http.StatusBadRequest, "environment is required")
		return
	}
	if f.EndsAt.IsZero() || f.StartsAt.IsZero() || !f.EndsAt.After(f.StartsAt) {
		writeErr(w, http.StatusBadRequest, "starts_at and ends_at are required and ends_at must be after starts_at")
		return
	}
	created, err := s.store.CreateFreeze(r.Context(), f)
	if err != nil {
		slog.Error("create freeze", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to create deploy freeze")
		return
	}
	s.audit(r, "deploy_freeze.create", strconv.FormatInt(created.ID, 10), f.Repo, "ok", map[string]any{
		"environment": f.Environment,
		"starts_at":   f.StartsAt,
		"ends_at":     f.EndsAt,
	})
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) deleteFreeze(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid freeze id")
		return
	}
	err := s.store.DeleteFreeze(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "deploy freeze not found")
		return
	}
	if err != nil {
		slog.Error("delete freeze", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to delete deploy freeze")
		return
	}
	s.audit(r, "deploy_freeze.delete", strconv.FormatInt(id, 10), "", "ok", nil)
	w.WriteHeader(http.StatusNoContent)
}
