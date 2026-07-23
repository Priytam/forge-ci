package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/compiler"
	"github.com/priytamjeepandey/forge-ci/internal/proto"
	"github.com/priytamjeepandey/forge-ci/internal/store"
)

// Forge is a standalone CI system — it does not host the repo. Pushes arrive
// via provider webhooks, and the pipeline YAML for each repo is registered
// with Forge (PUT /api/v1/repo-configs). See docs/vcs-integration.md.

func (s *Server) registerWebhookRoutes() {
	s.mux.HandleFunc("POST /api/v1/webhooks/github", s.githubWebhook)
	s.mux.HandleFunc("POST /api/v1/webhooks/bitbucket", s.bitbucketWebhook)
	s.mux.HandleFunc("PUT /api/v1/repo-configs", s.putRepoConfig)
	s.mux.HandleFunc("GET /api/v1/repo-configs", s.getRepoConfig)
	s.mux.HandleFunc("GET /api/v1/repo-configs/versions", s.listConfigVersions)
	s.mux.HandleFunc("POST /api/v1/repo-configs/revert", s.revertRepoConfig)
	s.mux.HandleFunc("POST /api/v1/repo-registry", s.registerRepo)
	s.mux.HandleFunc("GET /api/v1/repo-registry", s.listRegisteredRepos)
}

// registerRepo connects a Forge repo to a real GitHub/Bitbucket repository.
// Access is verified with git ls-remote before saving (skip with ?validate=0).
func (s *Server) registerRepo(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var req proto.RepoRegistration
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req.Repo = strings.TrimSpace(req.Repo)
	if req.Repo == "" {
		writeErr(w, http.StatusBadRequest, "repo (e.g. owner/name) is required")
		return
	}
	switch req.Provider {
	case "github":
		if req.CloneURL == "" {
			req.CloneURL = "https://github.com/" + req.Repo + ".git"
		}
	case "bitbucket":
		if req.CloneURL == "" {
			req.CloneURL = "https://bitbucket.org/" + req.Repo + ".git"
		}
	case "other":
		if req.CloneURL == "" {
			writeErr(w, http.StatusBadRequest, `provider "other" requires clone_url`)
			return
		}
	default:
		writeErr(w, http.StatusBadRequest, "provider must be github, bitbucket or other")
		return
	}
	if req.DefaultBranch == "" {
		req.DefaultBranch = "main"
	}

	if r.URL.Query().Get("validate") != "0" {
		if err := validateCloneAccess(r.Context(), req); err != nil {
			writeErr(w, http.StatusBadRequest,
				"could not reach the repository (check name, token and permissions): "+err.Error())
			return
		}
	}
	if err := s.store.RegisterRepo(r.Context(), req); err != nil {
		slog.Error("register repo", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to register repo")
		return
	}
	slog.Info("repo registered", "repo", req.Repo, "provider", req.Provider)
	w.WriteHeader(http.StatusNoContent)
}

// validateCloneAccess runs git ls-remote against the (possibly authenticated)
// URL. Errors are sanitized so tokens never reach the response.
func validateCloneAccess(ctx context.Context, req proto.RepoRegistration) error {
	u := req.CloneURL
	if req.Token != "" {
		parsed, err := url.Parse(u)
		if err == nil {
			if req.Provider == "bitbucket" {
				parsed.User = url.UserPassword("x-token-auth", req.Token)
			} else {
				parsed.User = url.UserPassword("x-access-token", req.Token)
			}
			u = parsed.String()
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "ls-remote", "--heads", u)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := string(out)
		if req.Token != "" {
			msg = strings.ReplaceAll(msg, req.Token, "[REDACTED]")
		}
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return errors.New(strings.TrimSpace(msg))
	}
	return nil
}

func (s *Server) listRegisteredRepos(w http.ResponseWriter, r *http.Request) {
	repos, err := s.store.ListRegisteredRepos(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to list registered repos")
		return
	}
	writeJSON(w, http.StatusOK, repos)
}

// triggerFromWebhook compiles the repo's registered config and creates a
// pipeline. It returns false (having written an error response) when nothing
// was created, so the caller can roll back a recorded webhook delivery.
func (s *Server) triggerFromWebhook(w http.ResponseWriter, r *http.Request, repo, ref, sha, author string) bool {
	if repo == "" || ref == "" || sha == "" {
		writeErr(w, http.StatusBadRequest, "payload missing repo/ref/sha")
		return false
	}
	config, err := s.store.GetRepoConfig(r.Context(), repo)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound,
			"no pipeline config registered for "+repo+" — PUT /api/v1/repo-configs first")
		return false
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to load repo config")
		return false
	}
	jobs, err := compiler.Compile(config, ref)
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, "pipeline config error: "+err.Error())
		return false
	}
	opts, err := compiler.Options(config)
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, "pipeline config error: "+err.Error())
		return false
	}
	var configVersion *int
	if v, verr := s.store.CurrentConfigVersion(r.Context(), repo); verr == nil && v > 0 {
		configVersion = &v
	}
	p, err := s.store.CreatePipeline(r.Context(),
		proto.CreatePipelineRequest{Repo: repo, Ref: ref, SHA: sha, Config: config, TriggeredBy: author},
		jobs, configVersion, opts.AutoCancel)
	if err != nil {
		slog.Error("webhook pipeline create", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to create pipeline")
		return false
	}
	slog.Info("pipeline triggered via webhook", "repo", repo, "ref", ref, "pipeline", p.ID)
	writeJSON(w, http.StatusCreated, map[string]any{"pipeline": p})
	return true
}

// dedupDelivery records this webhook delivery and reports whether processing
// should continue. On a duplicate it writes a 200 no-op and returns false. The
// returned finish func must be deferred: it rolls back the delivery record if
// the handler ends up creating nothing, so a genuine redelivery can retry.
func (s *Server) dedupDelivery(w http.ResponseWriter, r *http.Request, provider, deliveryID string) (proceed bool, created *bool) {
	isNew, err := s.store.RecordWebhookDelivery(r.Context(), provider, deliveryID)
	if err != nil {
		slog.Error("webhook dedup", "err", err)
		writeErr(w, http.StatusInternalServerError, "delivery dedup failed")
		return false, nil
	}
	if !isNew {
		slog.Info("webhook delivery is a duplicate, ignoring", "provider", provider, "delivery", deliveryID)
		writeJSON(w, http.StatusOK, map[string]string{"status": "duplicate delivery ignored"})
		return false, nil
	}
	var ok bool
	created = &ok
	return true, created
}

// githubWebhook handles push events. If WEBHOOK_SECRET is set, the
// X-Hub-Signature-256 HMAC is verified.
func (s *Server) githubWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "unreadable body")
		return
	}
	if secret := os.Getenv("WEBHOOK_SECRET"); secret != "" {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
		if !hmac.Equal([]byte(want), []byte(r.Header.Get("X-Hub-Signature-256"))) {
			writeErr(w, http.StatusUnauthorized, "invalid webhook signature")
			return
		}
	}
	if ev := r.Header.Get("X-GitHub-Event"); ev != "" && ev != "push" {
		w.WriteHeader(http.StatusAccepted) // ignore non-push events politely
		return
	}
	var payload struct {
		Ref        string `json:"ref"` // refs/heads/main
		After      string `json:"after"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		Pusher struct {
			Name string `json:"name"`
		} `json:"pusher"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}
	// Dedup: GitHub sends X-GitHub-Delivery; fall back to a hash of the body so
	// identical redeliveries without the header still dedup.
	deliveryID := r.Header.Get("X-GitHub-Delivery")
	if deliveryID == "" {
		deliveryID = bodyHash(body)
	}
	proceed, created := s.dedupDelivery(w, r, "github", deliveryID)
	if !proceed {
		return
	}
	defer func() {
		if !*created {
			_ = s.store.ForgetWebhookDelivery(r.Context(), "github", deliveryID)
		}
	}()
	ref := strings.TrimPrefix(payload.Ref, "refs/heads/")
	ref = strings.TrimPrefix(ref, "refs/tags/")
	*created = s.triggerFromWebhook(w, r, payload.Repository.FullName, ref, payload.After, payload.Pusher.Name)
}

// bodyHash is the fallback delivery id when a provider sends no delivery header.
func bodyHash(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// bitbucketWebhook handles repo:push events (Bitbucket Cloud payload shape).
func (s *Server) bitbucketWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "unreadable body")
		return
	}
	var payload struct {
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		Actor struct {
			Nickname    string `json:"nickname"`
			DisplayName string `json:"display_name"`
		} `json:"actor"`
		Push struct {
			Changes []struct {
				New struct {
					Name   string `json:"name"`
					Target struct {
						Hash string `json:"hash"`
					} `json:"target"`
				} `json:"new"`
			} `json:"changes"`
		} `json:"push"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}
	if len(payload.Push.Changes) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	// Dedup: Bitbucket sends X-Request-UUID; fall back to a hash of the body.
	deliveryID := r.Header.Get("X-Request-UUID")
	if deliveryID == "" {
		deliveryID = bodyHash(body)
	}
	proceed, created := s.dedupDelivery(w, r, "bitbucket", deliveryID)
	if !proceed {
		return
	}
	defer func() {
		if !*created {
			_ = s.store.ForgetWebhookDelivery(r.Context(), "bitbucket", deliveryID)
		}
	}()
	change := payload.Push.Changes[0].New
	author := payload.Actor.Nickname
	if author == "" {
		author = payload.Actor.DisplayName
	}
	*created = s.triggerFromWebhook(w, r, payload.Repository.FullName, change.Name, change.Target.Hash, author)
}

// ---- registered pipeline configs ----

// putRepoConfig pushes a NEW config version (append-only history) and moves
// the current pointer. Run-form edits never hit this endpoint.
func (s *Server) putRepoConfig(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var req struct {
		Repo    string `json:"repo"`
		Config  string `json:"config"`
		Author  string `json:"author"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Repo == "" || req.Config == "" {
		writeErr(w, http.StatusBadRequest, "repo and config are required")
		return
	}
	// Validate against a representative ref so broken YAML is rejected early.
	if _, err := compiler.Compile(req.Config, "main"); err != nil {
		writeErr(w, http.StatusBadRequest, "config error: "+err.Error())
		return
	}
	version, err := s.store.SetRepoConfig(r.Context(), req.Repo, req.Config, req.Author, req.Message)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to save config")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"repo": req.Repo, "version": version})
}

// getRepoConfig returns the current config, or a specific ?version=N.
func (s *Server) getRepoConfig(w http.ResponseWriter, r *http.Request) {
	repo := r.URL.Query().Get("repo")
	if repo == "" {
		writeErr(w, http.StatusBadRequest, "repo query param is required")
		return
	}
	if v := r.URL.Query().Get("version"); v != "" {
		var version int
		if _, err := fmt.Sscanf(v, "%d", &version); err != nil || version < 1 {
			writeErr(w, http.StatusBadRequest, "invalid version")
			return
		}
		config, err := s.store.GetConfigVersion(r.Context(), repo, version)
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "version not found")
			return
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "failed to load version")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"repo": repo, "config": config, "version": version})
		return
	}
	config, err := s.store.GetRepoConfig(r.Context(), repo)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "no config registered for this repo")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to load config")
		return
	}
	current, _ := s.store.CurrentConfigVersion(r.Context(), repo)
	writeJSON(w, http.StatusOK, map[string]any{"repo": repo, "config": config, "version": current})
}

func (s *Server) listConfigVersions(w http.ResponseWriter, r *http.Request) {
	repo := r.URL.Query().Get("repo")
	if repo == "" {
		writeErr(w, http.StatusBadRequest, "repo query param is required")
		return
	}
	versions, err := s.store.ListConfigVersions(r.Context(), repo)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to list versions")
		return
	}
	writeJSON(w, http.StatusOK, versions)
}

// revertRepoConfig copies an old version forward as a brand-new version —
// history is never rewritten, so the revert itself is auditable.
func (s *Server) revertRepoConfig(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var req struct {
		Repo    string `json:"repo"`
		Version int    `json:"version"`
		Author  string `json:"author"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Repo == "" || req.Version < 1 {
		writeErr(w, http.StatusBadRequest, "repo and version are required")
		return
	}
	config, err := s.store.GetConfigVersion(r.Context(), req.Repo, req.Version)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "version not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to load version")
		return
	}
	newVersion, err := s.store.SetRepoConfig(r.Context(), req.Repo, config, req.Author,
		fmt.Sprintf("revert to v%d", req.Version))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to revert")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"repo": req.Repo, "version": newVersion, "reverted_to": req.Version})
}
