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
	"strconv"
	"strings"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/compiler"
	"github.com/priytamjeepandey/forge-ci/internal/proto"
	"github.com/priytamjeepandey/forge-ci/internal/store"
	"github.com/priytamjeepandey/forge-ci/internal/vcs"
)

// Forge is a standalone CI system — it does not host the repo. Pushes arrive
// via provider webhooks, and the pipeline YAML for each repo is registered
// with Forge (PUT /api/v1/repo-configs). See docs/vcs-integration.md.

func (s *Server) registerWebhookRoutes() {
	s.mux.HandleFunc("POST /api/v1/webhooks/github", s.githubWebhook)
	s.mux.HandleFunc("POST /api/v1/webhooks/bitbucket", s.bitbucketWebhook)
	// CodeCommit has no webhooks: this route is fed by an EventBridge API
	// Destination. See internal/api/webhooks_codecommit.go.
	s.mux.HandleFunc("POST /api/v1/webhooks/codecommit", s.codecommitWebhook)
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

// registerRepo connects a Forge repo to a real GitHub/Bitbucket/CodeCommit
// repository. Access is verified before saving (skip with ?validate=0) — with
// git ls-remote for the HTTPS providers, and with the CodeCommit API for
// CodeCommit, which has no token for git to use.
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
	case "codecommit":
		if err := normalizeCodeCommitRegistration(&req); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	case "other":
		if req.CloneURL == "" {
			writeErr(w, http.StatusBadRequest, `provider "other" requires clone_url`)
			return
		}
	default:
		writeErr(w, http.StatusBadRequest, "provider must be github, bitbucket, codecommit or other")
		return
	}
	if req.DefaultBranch == "" {
		req.DefaultBranch = "main"
	}

	// config-from-repo toggle: 'repo' (default) prefers the in-repo .forge-ci.yml
	// and falls back to the registered config; 'registered' only ever uses the
	// registered config. config_path overrides the fetched path.
	if req.ConfigSource == "" {
		req.ConfigSource = "repo"
	}
	if req.ConfigSource != "repo" && req.ConfigSource != "registered" {
		writeErr(w, http.StatusBadRequest, `config_source must be "repo" or "registered"`)
		return
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
		if req.Provider == "codecommit" {
			// git ls-remote cannot reach CodeCommit without IAM-signed git auth on
			// the control-plane host, which Forge does not require. The CodeCommit
			// API is the honest equivalent: it proves the same identity, region and
			// repository name resolve, using the very call config-from-repo makes.
			if err := s.store.ValidateCodeCommitAccess(r.Context(), req); err != nil {
				writeErr(w, http.StatusBadRequest,
					"could not reach the CodeCommit repository "+
						"(check aws_region, the repository name, and that the control plane's "+
						"AWS identity has codecommit:GetRepository): "+err.Error())
				return
			}
		} else if appConfigured {
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
		"config_source":  req.ConfigSource,
		"config_path":    req.ConfigPath,
	}
	if appConfigured {
		detail["github_app_id"] = req.GitHubAppID
		detail["installation_id"] = req.GitHubInstallationID
		// The private key is NEVER recorded.
	}
	if req.Provider == "codecommit" {
		// All three are non-secret: they name where the repo lives and which role
		// to wear, never a credential.
		detail["aws_region"] = req.AWSRegion
		detail["aws_profile"] = req.AWSProfile
		detail["aws_role_arn"] = req.AWSRoleARN
	}
	return detail
}

// normalizeCodeCommitRegistration validates and fills in a CodeCommit
// registration in place, so the rules are testable without standing up a server.
//
// CodeCommit needs a region, not a token. The region may arrive explicitly or
// encoded in a clone URL of either accepted form; whichever way it comes, both
// the region field and the clone URL are filled in from the other, so the
// registry record and the derived URL can never disagree later.
func normalizeCodeCommitRegistration(req *proto.RepoRegistration) error {
	req.AWSRegion = strings.TrimSpace(req.AWSRegion)
	req.AWSProfile = strings.TrimSpace(req.AWSProfile)
	req.AWSRoleARN = strings.TrimSpace(req.AWSRoleARN)
	req.CloneURL = strings.TrimSpace(req.CloneURL)

	// Nothing secret is ever stored for a CodeCommit repo. A token here would be
	// dead weight at best and a needlessly stored credential at worst.
	if req.Token != "" {
		return errors.New(`provider "codecommit" takes no token — access is IAM-signed ` +
			"(set aws_role_arn to assume a role instead)")
	}
	// The Forge repo key IS the CodeCommit repository name; an owner/name key
	// would never match the repositoryName an EventBridge event carries.
	if strings.Contains(req.Repo, "/") {
		return errors.New("a CodeCommit repo key is the repository name alone, " +
			"with no owner/ prefix")
	}
	if parsed, ok := vcs.ParseCodeCommitURL(req.CloneURL); ok {
		if req.AWSRegion == "" {
			req.AWSRegion = parsed.Region
		}
		if req.AWSProfile == "" {
			req.AWSProfile = parsed.Profile
		}
	} else if req.CloneURL != "" {
		return errors.New(`provider "codecommit" clone_url must be ` +
			"codecommit::<region>://<repo> or " +
			"https://git-codecommit.<region>.amazonaws.com/v1/repos/<repo>")
	}
	if req.AWSRegion == "" {
		return errors.New(`provider "codecommit" requires aws_region ` +
			"(or a clone_url that encodes it)")
	}
	if req.CloneURL == "" {
		req.CloneURL = vcs.CodeCommitCloneURL(req.AWSRegion, req.Repo)
	}
	return nil
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
//
// source is the pipeline source threaded into rules if: as CI_PIPELINE_SOURCE
// (push webhooks pass SourceWebhook; PR/MR webhooks pass SourceMergeRequest).
// extraCtx carries source-specific context (the CI_MERGE_REQUEST_* vars for a
// merge_request pipeline); it is visible to rules if: AND injected into every
// job's Env. pr, when non-nil, records which pull request the run belongs to so
// a provider with no commit-status API (CodeCommit) can comment back on it; it
// is nil for push pipelines. For a PR pipeline the caller passes the PR HEAD sha and the PR head
// branch as ref, so only/except and rules matching the branch still work and the
// commit status lands on the PR head sha.
func (s *Server) triggerFromWebhook(w http.ResponseWriter, r *http.Request, repo, ref, sha, author, source string, extraCtx map[string]string, pr *prSource) bool {
	if repo == "" || ref == "" || sha == "" {
		writeErr(w, http.StatusBadRequest, "payload missing repo/ref/sha")
		return false
	}
	// config-from-repo: resolve the config for this event per the repo's
	// config_source toggle — an in-repo .forge-ci.yml fetched at the event sha
	// (preferred by default), else the registered config. configVersion is nil
	// for an in-repo config (it is not a registry version); cfgSource is stamped
	// on the pipeline for audit. config_yaml stores exactly what compiled.
	config, configVersion, cfgSource, err := s.store.ResolvePipelineConfig(r.Context(), repo, sha)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound,
			"no pipeline config for "+repo+" — add .forge-ci.yml to the repo or PUT /api/v1/repo-configs first")
		return false
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to load repo config")
		return false
	}
	jobs, err := compiler.Compile(config, ref, source, s.store.TemplateResolver(r.Context(), repo), extraCtx)
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, "pipeline config error: "+err.Error())
		return false
	}
	opts, err := compiler.Options(config)
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, "pipeline config error: "+err.Error())
		return false
	}
	create := proto.CreatePipelineRequest{
		Repo: repo, Ref: ref, SHA: sha, Config: config,
		TriggeredBy: author, ConfigSource: cfgSource, Source: source,
	}
	if pr != nil {
		create.MRIID, create.MRBaseSHA = pr.IID, pr.BaseSHA
	}
	p, err := s.store.CreatePipeline(r.Context(), create,
		jobs, configVersion, opts.AutoCancel, opts.FailFast)
	if err != nil {
		slog.Error("webhook pipeline create", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to create pipeline")
		return false
	}
	slog.Info("pipeline triggered via webhook", "repo", repo, "ref", ref, "pipeline", p.ID, "config_source", cfgSource)
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

// githubWebhook handles push and pull_request events. If WEBHOOK_SECRET is set,
// the X-Hub-Signature-256 HMAC is verified. The X-GitHub-Event header selects
// the handler; any other event is acknowledged (202) and ignored.
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
	switch r.Header.Get("X-GitHub-Event") {
	case "", "push":
		s.githubPush(w, r, body)
	case "pull_request":
		s.githubPullRequest(w, r, body)
	default:
		w.WriteHeader(http.StatusAccepted) // ignore other events politely
	}
}

// githubPush creates a pipeline for a branch/tag push at the pushed sha.
func (s *Server) githubPush(w http.ResponseWriter, r *http.Request, body []byte) {
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
	*created = s.triggerFromWebhook(w, r, payload.Repository.FullName, ref, payload.After,
		payload.Pusher.Name, compiler.SourceWebhook, nil, nil)
}

// prSource is the pull-request identity recorded on a merge_request pipeline.
// GitHub and Bitbucket report results through a real commit-status API and only
// need the id for reference; CodeCommit has no status API at all and needs both
// the id and the base commit to post a comment on the PR later.
type prSource struct {
	IID     string // provider pull request id
	BaseSHA string // destination-branch commit the PR targets ("before")
}

// prTrigger is the normalized subset of a PR/MR webhook payload needed to build
// a merge_request pipeline, shared by the GitHub and Bitbucket PR handlers.
type prTrigger struct {
	Action       string // provider action (GitHub); "" for Bitbucket (event key gates instead)
	Repo         string // owner/name
	IID          int    // PR/MR number
	Title        string
	SourceBranch string // PR head branch — used as the compile ref
	TargetBranch string // PR base branch
	HeadSHA      string // PR head commit — the pipeline sha (status lands here)
	Author       string
}

// parseGitHubPR extracts a prTrigger from a GitHub pull_request payload.
func parseGitHubPR(body []byte) (prTrigger, error) {
	var payload struct {
		Action      string `json:"action"`
		Number      int    `json:"number"`
		PullRequest struct {
			Title string `json:"title"`
			Head  struct {
				SHA string `json:"sha"`
				Ref string `json:"ref"`
			} `json:"head"`
			Base struct {
				Ref string `json:"ref"`
			} `json:"base"`
			User struct {
				Login string `json:"login"`
			} `json:"user"`
		} `json:"pull_request"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return prTrigger{}, err
	}
	pr := payload.PullRequest
	return prTrigger{
		Action:       payload.Action,
		Repo:         payload.Repository.FullName,
		IID:          payload.Number,
		Title:        pr.Title,
		SourceBranch: pr.Head.Ref,
		TargetBranch: pr.Base.Ref,
		HeadSHA:      pr.Head.SHA,
		Author:       pr.User.Login,
	}, nil
}

// githubPullRequest creates a merge_request pipeline for a pull_request event.
// It runs only on the opened / synchronize / reopened actions (all others are
// acknowledged with 202 and ignored). The pipeline is built on the PR HEAD sha
// with the PR head branch as the compile ref — so branch-keyed only/except and
// rules still apply and the commit status lands on the PR head — while
// CI_PIPELINE_SOURCE is forced to merge_request and the CI_MERGE_REQUEST_* vars
// are threaded into rules and job env.
func (s *Server) githubPullRequest(w http.ResponseWriter, r *http.Request, body []byte) {
	pr, err := parseGitHubPR(body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}
	if !prActionTriggers(pr.Action) {
		w.WriteHeader(http.StatusAccepted) // opened/synchronize/reopened only
		return
	}
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
	ctx := mergeRequestContext(pr.IID, pr.SourceBranch, pr.TargetBranch, pr.Title)
	*created = s.triggerFromWebhook(w, r, pr.Repo,
		pr.SourceBranch, pr.HeadSHA, pr.Author, compiler.SourceMergeRequest, ctx,
		&prSource{IID: strconv.Itoa(pr.IID)})
}

// prActionTriggers reports whether a GitHub pull_request / Bitbucket pull-request
// action should (re)build a pipeline. Only opened, (re)synchronize and reopened
// do; closed/merged/edited/labeled/etc. are ignored.
func prActionTriggers(action string) bool {
	switch action {
	case "opened", "synchronize", "reopened":
		return true
	}
	return false
}

// mergeRequestContext builds the CI_MERGE_REQUEST_* context for a PR/MR pipeline.
// It also pins CI_PIPELINE_SOURCE=merge_request so the value is present in job env
// (Compile already sets it in the rules context from the source argument).
func mergeRequestContext(iid int, sourceBranch, targetBranch, title string) map[string]string {
	return map[string]string{
		"CI_PIPELINE_SOURCE":             compiler.SourceMergeRequest,
		"CI_MERGE_REQUEST_IID":           strconv.Itoa(iid),
		"CI_MERGE_REQUEST_SOURCE_BRANCH": sourceBranch,
		"CI_MERGE_REQUEST_TARGET_BRANCH": targetBranch,
		"CI_MERGE_REQUEST_TITLE":         title,
	}
}

// bodyHash is the fallback delivery id when a provider sends no delivery header.
func bodyHash(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// bitbucketWebhook handles repo:push and pull-request events (Bitbucket Cloud
// payload shape). The X-Event-Key header selects the handler; any other event is
// acknowledged (202) and ignored.
func (s *Server) bitbucketWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "unreadable body")
		return
	}
	switch r.Header.Get("X-Event-Key") {
	case "", "repo:push":
		s.bitbucketPush(w, r, body)
	case "pullrequest:created", "pullrequest:updated":
		s.bitbucketPullRequest(w, r, body)
	default:
		w.WriteHeader(http.StatusAccepted) // ignore other events politely
	}
}

// bitbucketPush creates a pipeline for a repo:push at the pushed sha.
func (s *Server) bitbucketPush(w http.ResponseWriter, r *http.Request, body []byte) {
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
	*created = s.triggerFromWebhook(w, r, payload.Repository.FullName, change.Name,
		change.Target.Hash, author, compiler.SourceWebhook, nil, nil)
}

// parseBitbucketPR extracts a prTrigger from a Bitbucket pull-request payload.
// Action is left empty — the X-Event-Key header (not a payload field) gates which
// Bitbucket PR events trigger a build.
func parseBitbucketPR(body []byte) (prTrigger, error) {
	var payload struct {
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		Actor struct {
			Nickname    string `json:"nickname"`
			DisplayName string `json:"display_name"`
		} `json:"actor"`
		PullRequest struct {
			ID     int    `json:"id"`
			Title  string `json:"title"`
			Source struct {
				Branch struct {
					Name string `json:"name"`
				} `json:"branch"`
				Commit struct {
					Hash string `json:"hash"`
				} `json:"commit"`
			} `json:"source"`
			Destination struct {
				Branch struct {
					Name string `json:"name"`
				} `json:"branch"`
			} `json:"destination"`
		} `json:"pullrequest"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return prTrigger{}, err
	}
	author := payload.Actor.Nickname
	if author == "" {
		author = payload.Actor.DisplayName
	}
	pr := payload.PullRequest
	return prTrigger{
		Repo:         payload.Repository.FullName,
		IID:          pr.ID,
		Title:        pr.Title,
		SourceBranch: pr.Source.Branch.Name,
		TargetBranch: pr.Destination.Branch.Name,
		HeadSHA:      pr.Source.Commit.Hash,
		Author:       author,
	}, nil
}

// bitbucketPullRequest creates a merge_request pipeline for a pullrequest:created
// or pullrequest:updated event, built on the PR source-branch tip commit with the
// source branch as the compile ref (same head-sha / branch-ref semantics as the
// GitHub PR path). CI_PIPELINE_SOURCE=merge_request and the CI_MERGE_REQUEST_*
// vars are threaded into rules and job env.
func (s *Server) bitbucketPullRequest(w http.ResponseWriter, r *http.Request, body []byte) {
	pr, err := parseBitbucketPR(body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON payload")
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
	ctx := mergeRequestContext(pr.IID, pr.SourceBranch, pr.TargetBranch, pr.Title)
	*created = s.triggerFromWebhook(w, r, pr.Repo,
		pr.SourceBranch, pr.HeadSHA, pr.Author, compiler.SourceMergeRequest, ctx,
		&prSource{IID: strconv.Itoa(pr.IID)})
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
