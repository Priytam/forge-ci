// Package proto holds the JSON types shared by the server API, the runner,
// and (structurally) the web frontend.
package proto

import "time"

type Pipeline struct {
	ID     int64         `json:"id"`
	Repo   string        `json:"repo"`
	Ref    string        `json:"ref"`
	SHA    string        `json:"sha"`
	Status string        `json:"status"`
	Stages []StageStatus `json:"stages"`
	// ConfigVersion is the registered config version this pipeline ran;
	// nil means a one-off custom config was supplied at run time.
	ConfigVersion *int      `json:"config_version"`
	CreatedAt     time.Time `json:"created_at"`
}

// StageStatus is the derived status of one stage, for the mini per-stage
// indicators in list views.
type StageStatus struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// RepoSummary is the top-level card for a repo: Forge doesn't own the repo,
// so this aggregates what it has seen via pipelines.
type RepoSummary struct {
	Repo           string    `json:"repo"`
	PipelineCount  int       `json:"pipeline_count"`
	SuccessCount   int       `json:"success_count"`
	FailedCount    int       `json:"failed_count"`
	Refs           []string  `json:"refs"`            // distinct refs, most recent first
	RecentStatuses []string  `json:"recent_statuses"` // last pipelines' statuses, newest first
	LastPipeline   *Pipeline `json:"last_pipeline"`   // includes stages
	LastActivityAt time.Time `json:"last_activity_at"`
}

type Job struct {
	ID          int64      `json:"id"`
	PipelineID  int64      `json:"pipeline_id"`
	Name        string     `json:"name"`
	Stage       string     `json:"stage"`
	StageIdx    int        `json:"stage_idx"`
	Image       *string    `json:"image"`
	Environment *string    `json:"environment"`
	Status      string     `json:"status"`
	StartedAt   *time.Time `json:"started_at"`
	FinishedAt  *time.Time `json:"finished_at"`
	ExitCode    *int       `json:"exit_code"`
	Needs       []int64    `json:"needs"`
}

// RunnerJob is the payload handed to a runner when it acquires a job.
type RunnerJob struct {
	ID         int64             `json:"id"`
	PipelineID int64             `json:"pipeline_id"`
	Name       string            `json:"name"`
	Image      string            `json:"image"`
	Script     string            `json:"script"`
	Env        map[string]string `json:"env"`

	ArtifactPaths []string `json:"artifact_paths"`

	// Cache directives (GitLab-style). The runner restores the cache before the
	// script and saves it after, keyed by repo+key and shared across pipelines.
	// CachePaths empty means the job declares no cache. CacheKeyFiles, when set,
	// are hashed by the runner (after checkout) into a content-addressed key.
	CachePaths    []string `json:"cache_paths,omitempty"`
	CacheKey      string   `json:"cache_key,omitempty"`
	CacheKeyFiles []string `json:"cache_key_files,omitempty"`
	CachePolicy   string   `json:"cache_policy,omitempty"` // pull | push | pull-push

	// TimeoutSeconds is the resolved execution timeout (job value clamped to
	// the server cap, or the server default). The runner kills the job when
	// it elapses; the scheduler backstops with a grace period.
	TimeoutSeconds int `json:"timeout_seconds"`

	// Source checkout: when CloneURL is set (repo is registered) the runner
	// clones SHA into the workspace before running. RedactValues must never
	// appear in logs (embedded tokens).
	CloneURL     string   `json:"clone_url,omitempty"`
	SHA          string   `json:"sha,omitempty"`
	Ref          string   `json:"ref,omitempty"`
	RepoName     string   `json:"repo_name,omitempty"`
	RedactValues []string `json:"redact_values,omitempty"`

	// Artifacts of the jobs this job needs — restored into the workspace
	// before the script runs (GitLab-style artifact passing).
	Dependencies []DependencyArtifact `json:"dependencies,omitempty"`
}

// DependencyArtifact points a runner at an upstream job's artifact archive.
type DependencyArtifact struct {
	ArtifactID int64  `json:"artifact_id"`
	JobName    string `json:"job_name"`
	Name       string `json:"name"`
}

// RepoRegistration connects a Forge repo to its real VCS repository. A
// connection authenticates via EITHER a static token (PAT) OR a GitHub App
// (app id + private key + installation id); when App fields are configured they
// take precedence and Forge mints short-lived installation tokens on demand.
type RepoRegistration struct {
	Repo          string    `json:"repo"`
	Provider      string    `json:"provider"` // github | bitbucket | other
	CloneURL      string    `json:"clone_url"`
	Token         string    `json:"token,omitempty"` // write-only; never returned
	HasToken      bool      `json:"has_token"`
	DefaultBranch string    `json:"default_branch"`
	CreatedAt     time.Time `json:"created_at"`

	// GitHub App auth (github only). GitHubAppPrivateKey is a secret: write-only,
	// encrypted at rest, and NEVER populated in responses. AppID and
	// InstallationID are not secret and are returned so the config is visible.
	GitHubAppID          string `json:"github_app_id,omitempty"`
	GitHubInstallationID string `json:"github_installation_id,omitempty"`
	GitHubAppPrivateKey  string `json:"github_app_private_key,omitempty"` // write-only; never returned
	HasGitHubApp         bool   `json:"has_github_app"`
}

// Runner is a registered build agent as seen by the control plane.
type Runner struct {
	ID            string    `json:"id"`
	Executor      string    `json:"executor"`
	Tags          []string  `json:"tags"`
	Description   string    `json:"description"`
	Paused        bool      `json:"paused"`
	Online        bool      `json:"online"` // contacted within the last 30s
	CreatedAt     time.Time `json:"created_at"`
	LastContactAt time.Time `json:"last_contact_at"`
}

// ArtifactInfo is artifact metadata for list/download endpoints.
type ArtifactInfo struct {
	ID         int64     `json:"id"`
	JobID      int64     `json:"job_id"`
	JobName    string    `json:"job_name"`
	PipelineID int64     `json:"pipeline_id"`
	Repo       string    `json:"repo"`
	Name       string    `json:"name"`
	SizeBytes  int64     `json:"size_bytes"`
	CreatedAt  time.Time `json:"created_at"`
}

// Variable is a repo-scoped CI/CD variable. Value is redacted ("") in list
// responses when masked, unless reveal was requested.
type Variable struct {
	ID               int64     `json:"id"`
	Repo             string    `json:"repo"`
	Key              string    `json:"key"`
	Value            string    `json:"value"`
	Protected        bool      `json:"protected"`
	Masked           bool      `json:"masked"`
	EnvironmentScope string    `json:"environment_scope"`
	CreatedAt        time.Time `json:"created_at"`
}

type VariableRequest struct {
	Repo             string `json:"repo"`
	Key              string `json:"key"`
	Value            string `json:"value"`
	Protected        bool   `json:"protected"`
	Masked           bool   `json:"masked"`
	EnvironmentScope string `json:"environment_scope"`
}

type AcquireRequest struct {
	RunnerID string   `json:"runner_id"`
	Executor string   `json:"executor"`
	Tags     []string `json:"tags"`
}

type CompleteRequest struct {
	Status   string `json:"status"` // success | failed | canceled | requeue
	ExitCode int    `json:"exit_code"`
}

// HeartbeatResponse is returned to the runner on every heartbeat. Cancel=true
// tells the runner to stop the job and report completion as 'canceled'.
type HeartbeatResponse struct {
	Cancel bool `json:"cancel"`
}

type ApprovalRequest struct {
	Approver string `json:"approver"`
	Verdict  string `json:"verdict"` // approved | rejected
	Comment  string `json:"comment,omitempty"`
}

type CreatePipelineRequest struct {
	Repo        string `json:"repo"`
	Ref         string `json:"ref"`
	SHA         string `json:"sha"`
	Config      string `json:"config"`
	TriggeredBy string `json:"triggered_by,omitempty"`
}

// Member is a repo membership row (RBAC).
type Member struct {
	ID       int64  `json:"id"`
	Repo     string `json:"repo"`
	Username string `json:"username"`
	Role     string `json:"role"` // admin | owner | developer
}

// ProtectedEnvironment is a per-repo (or global, repo="") approval rule.
type ProtectedEnvironment struct {
	ID                   int64    `json:"id"`
	Repo                 string   `json:"repo"`
	Name                 string   `json:"name"`
	RequiredApprovals    int      `json:"required_approvals"`
	ApprovalTimeoutHours int      `json:"approval_timeout_hours"`
	ApproverRoles        []string `json:"approver_roles"`
	AllowSelfApproval    bool     `json:"allow_self_approval"`
}
