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
	s.mux.HandleFunc("PUT /api/v1/repo-templates", s.putRepoTemplate)
	s.mux.HandleFunc("GET /api/v1/repo-templates", s.listRepoTemplates)
}

// putRepoTemplate registers (or replaces) a reusable pipeline fragment that
// top-level include: [{template: name}] can compose in. Forge hosts no repo
// file tree, so include: resolves against these named per-repo templates.
func (s *Server) putRepoTemplate(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var req struct {
		Repo string `json:"repo"`
		Name string `json:"name"`
		YAML string `json:"yaml"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Repo == "" || req.Name == "" || req.YAML == "" {
		writeErr(w, http.StatusBadRequest, "repo, name and yaml are required")
		return
	}
	if err := s.store.UpsertRepoTemplate(r.Context(), req.Repo, req.Name, req.YAML); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to save template")
		return
	}
	s.audit(r, "repo-template.push", req.Name, req.Repo, "ok", map[string]any{"name": req.Name})
	writeJSON(w, http.StatusOK, map[string]any{"repo": req.Repo, "name": req.Name})
}

// listRepoTemplates lists the names of a repo's registered templates.
func (s *Server) listRepoTemplates(w http.ResponseWriter, r *http.Request) {
	repo := r.URL.Query().Get("repo")
	if repo == "" {
		writeErr(w, http.StatusBadRequest, "repo query param is required")
		return
	}
	names, err := s.store.ListRepoTemplates(r.Context(), repo)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to list templates")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"repo": repo, "templates": names})
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

	// GitHub App auth (github only) is configured when any App field is present.
	// It authenticates INSTEAD of a static token — Forge mints short-lived
	// installation tokens on demand (see internal/githubapp).
	appConfigured := req.GitHubAppID != "" || req.GitHubInstallationID != "" || req.GitHubAppPrivateKey != ""
	if appConfigured {
		if req.Provider != "github" {
			writeErr(w, http.StatusBadRequest, `GitHub App auth requires provider "github"`)
			return
		}
		if req.GitHubAppID == "" || req.GitHubInstallationID == "" {
			writeErr(w, http.StatusBadRequest,
				"github_app_id and github_installation_id are required for GitHub App auth")
			return
		}
	}

	if r.URL.Query().Get("validate") != "0" {
		if appConfigured {
			// Prove the App config works: mint an installation token, then
			// ls-remote with it. Errors never contain the key or token.
			token, err := s.store.AppTokenForValidation(r.Context(),
				req.Repo, req.GitHubAppID, req.GitHubAppPrivateKey, req.GitHubInstallationID)
			if err != nil {
				writeErr(w, http.StatusBadRequest,
					"could not mint a GitHub App installation token "+
						"(check app id, installation id and private key): "+err.Error())
				return
			}
			if err := validateCloneAccessWithToken(r.Context(), req.CloneURL, token, "github"); err != nil {
				writeErr(w, http.StatusBadRequest,
					"could not reach the repository with the App token "+
						"(check the installation and its Contents:read permission): "+err.Error())
				return
			}
		} else if err := validateCloneAccess(r.Context(), req); err != nil {
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
	slog.Info("repo registered", "repo", req.Repo, "provider", req.Provider, "github_app", appConfigured)
	// NEVER record the token or the private key.
	s.audit(r, "repo-registry.register", req.Repo, req.Repo, "ok", repoRegistryAuditDetail(req))
	w.WriteHeader(http.StatusNoContent)
}

// repoRegistryAuditDetail builds the safe audit detail for a repo registration.
// It records only non-secret metadata: NEVER the static token and NEVER the
// GitHub App private key. has_token / github_app note that a secret was set,
// and app_id / installation_id (not secret) identify the App connection.
func repoRegistryAuditDetail(req proto.RepoRegistration) map[string]any {
	appConfigured := req.GitHubAppID != "" || req.GitHubInstallationID != "" || req.GitHubAppPrivateKey != ""
	detail := map[string]any{
		"provider":       req.Provider,
		"clone_url":      req.CloneURL,
		"default_branch": req.DefaultBranch,
		"has_token":      req.Token != "",
		"github_app":     appConfigured,
	}
	if appConfigured {
		detail["github_app_id"] = req.GitHubAppID
		detail["installation_id"] = req.GitHubInstallationID
		// The private key is NEVER recorded.
	}
	return detail
}

// validateCloneAccess runs git ls-remote against the PAT-authenticated URL.
// Errors are sanitized so tokens never reach the response.
func validateCloneAccess(ctx context.Context, req proto.RepoRegistration) error {
	return validateCloneAccessWithToken(ctx, req.CloneURL, req.Token, req.Provider)
}

// validateCloneAccessWithToken runs git ls-remote against cloneURL, embedding
// token (a static PAT or a minted GitHub App installation token) as the provider
// expects. token is redacted from any error text so it never reaches the
// response. An empty token validates anonymous (public-repo) access.
func validateCloneAccessWithToken(ctx context.Context, cloneURL, token, provider string) error {
	u := cloneURL
	if token != "" {
		parsed, err := url.Parse(u)
		if err == nil {
			if provider == "bitbucket" {
				parsed.User = url.UserPassword("x-token-auth", token)
			} else {
				parsed.User = url.UserPassword("x-access-token", token)
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
		if token != "" {
			msg = strings.ReplaceAll(msg, token, "[REDACTED]")
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
	jobs, err := compiler.Compile(config, ref, compiler.SourceWebhook, s.store.TemplateResolver(r.Context(), repo))
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
	// Templates referenced via include: are resolved against this repo's
	// registered templates so an include of a missing template is caught here.
	if _, err := compiler.Compile(req.Config, "main", compiler.SourcePush, s.store.TemplateResolver(r.Context(), req.Repo)); err != nil {
		writeErr(w, http.StatusBadRequest, "config error: "+err.Error())
		return
	}
	// Identity-from-session: when SSO is enforced the config author is the
	// AUTHENTICATED identity — a client-supplied author is ignored. In open
	// bootstrap mode the client value is preserved.
	if actor := s.sessionActor(r); actor != "" {
		req.Author = actor
	}
	version, err := s.store.SetRepoConfig(r.Context(), req.Repo, req.Config, req.Author, req.Message)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to save config")
		return
	}
	s.audit(r, "repo-config.push", req.Repo, req.Repo, "ok", map[string]any{
		"version": version,
		"author":  req.Author,
		"message": req.Message,
	})
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
	// Identity-from-session: the revert author is the authenticated identity
	// when SSO is enforced; the client value is kept only in open bootstrap mode.
	if actor := s.sessionActor(r); actor != "" {
		req.Author = actor
	}
	newVersion, err := s.store.SetRepoConfig(r.Context(), req.Repo, config, req.Author,
		fmt.Sprintf("revert to v%d", req.Version))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to revert")
		return
	}
	s.audit(r, "repo-config.revert", req.Repo, req.Repo, "ok", map[string]any{
		"version":     newVersion,
		"reverted_to": req.Version,
		"author":      req.Author,
	})
	writeJSON(w, http.StatusOK, map[string]any{"repo": req.Repo, "version": newVersion, "reverted_to": req.Version})
}
