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
	ConfigVersion *int `json:"config_version"`
	// ConfigSource is where the config that ran came from: 'repo' (the in-repo
	// .forge-ci.yml fetched at SHA) or 'registered' (Forge's own registry).
	ConfigSource string `json:"config_source,omitempty"`
	// ConfigURL deep-links the config file this run used, in the provider's web
	// UI at SHA. Empty when no correct link can be built — see
	// Store.ConfigFileURL — and the UI then simply shows none.
	ConfigURL string `json:"config_url,omitempty"`
	// Source is the trigger: api | push | webhook | merge_request | schedule.
	Source string `json:"source"`
	// TriggeredBy is the identity that started the run — the pusher/PR author
	// for a webhook, the caller for an API run, "" for a schedule (nobody did).
	TriggeredBy string `json:"triggered_by,omitempty"`
	// CommitAuthor is who WROTE the code, which is not always who started the
	// run: a merge, a rebase, a bot push or a re-run of an old commit all have
	// an actor who is not the author. "" when the provider event does not say.
	CommitAuthor string `json:"commit_author,omitempty"`
	// CommitMessage is what the run is about — the commit message for a push,
	// the pull-request title for a PR run.
	CommitMessage string    `json:"commit_message,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	// Coverage is the mean of the pipeline's job coverage values (nil = none
	// reported). PromotedFrom is the deployment a promotion pipeline redeploys.
	Coverage     *float64 `json:"coverage,omitempty"`
	PromotedFrom *int64   `json:"promoted_from,omitempty"`
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
	Refs           []string  `json:"refs"`            // distinct recent refs, most recent first
	RefCount       int       `json:"ref_count"`       // distinct refs among recent runs (Refs may be cut short)
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
	// Manual is true for a when:manual gate (released via POST /jobs/{id}/play),
	// distinguishing it from an environment-approval block. AllowFailure is true
	// when the job's failure does not fail dependents or the pipeline.
	Manual       bool `json:"manual"`
	AllowFailure bool `json:"allow_failure"`
	// StatusReason says why a job ended without running — e.g. its branch may
	// not deploy to its environment. Empty otherwise.
	StatusReason string `json:"status_reason,omitempty"`
	// RetryOf is the job this one re-runs (POST /jobs/{id}/retry), if any.
	RetryOf *int64 `json:"retry_of,omitempty"`
	// ResourceGroup: at most one job per (repo, group) runs at a time.
	ResourceGroup string `json:"resource_group,omitempty"`
	// Coverage is the job's coverage percentage, when it reports one.
	Coverage *float64 `json:"coverage,omitempty"`
	// RunnerID is the runner that ran (or is running) the job.
	RunnerID *string `json:"runner_id,omitempty"`

	// Approval gate, populated only for a job that targets a protected
	// environment. Approvals is the append-only vote record (one per approver,
	// oldest first) and RequiredApprovals the count the gate needs, so a
	// reviewer can see where the gate stands before voting. Both are zero for a
	// job with no gate.
	Approvals         []JobApproval `json:"approvals,omitempty"`
	RequiredApprovals int           `json:"required_approvals,omitempty"`
	// RequiredGroup / RequiredGroupApprovals: the gate also needs this many
	// approvals from members of this approver group.
	RequiredGroup          string `json:"required_group,omitempty"`
	RequiredGroupApprovals int    `json:"required_group_approvals,omitempty"`
	// GroupApprovals: approved votes so far from the required group;
	// GroupMembers: who could still supply one (the group's members).
	GroupApprovals int      `json:"group_approvals,omitempty"`
	GroupMembers   []string `json:"group_members,omitempty"`
}

// JobApproval is one recorded vote on an approval-gated job. Votes are
// append-only and unique per approver, so this doubles as the audit record of
// who released a deployment.
type JobApproval struct {
	Approver  string    `json:"approver"`
	Verdict   string    `json:"verdict"` // approved | rejected
	Comment   string    `json:"comment,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// RunnerJob is the payload handed to a runner when it acquires a job.
type RunnerJob struct {
	// RunDir is runner-local (never sent over the wire): a per-job directory
	// for files the runner keeps fresh while the job runs, such as the OIDC
	// token file. Executors expose it to the job (see executor.RunPath).
	RunDir string `json:"-"`

	ID         int64             `json:"id"`
	PipelineID int64             `json:"pipeline_id"`
	Name       string            `json:"name"`
	Image      string            `json:"image"`
	Script     string            `json:"script"`
	Env        map[string]string `json:"env"`

	ArtifactPaths []string `json:"artifact_paths"`

	// ReportJUnitPaths are workspace globs of JUnit XML the runner collects after
	// a successful job and POSTs to the report endpoint for server-side parsing.
	ReportJUnitPaths []string `json:"report_junit_paths,omitempty"`
	// ReportDotenvPath is a KEY=VALUE file the runner reads after a successful
	// job and posts to /runner/jobs/{id}/dotenv; CoverageReportPath is a
	// Cobertura file posted to /runner/jobs/{id}/coverage. Both workspace-relative.
	ReportDotenvPath   string `json:"report_dotenv_path,omitempty"`
	CoverageReportPath string `json:"coverage_report_path,omitempty"`
	// Secrets are fetched from external secret managers before the script
	// (see compiler.secretSpec); RegistryLogins are registries to log into.
	Secrets        map[string]SecretSpec `json:"secrets,omitempty"`
	RegistryLogins []string              `json:"registry_logins,omitempty"`
	// DockerConfigDir is runner-local: the directory holding the Docker
	// config.json the runner prepared for this job ("" = none).
	DockerConfigDir string `json:"-"`

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

	// CloneAuth selects how the runner authenticates the checkout. Empty means
	// the historical behaviour: CloneURL already carries whatever credential is
	// needed (an embedded token, or none for a public repo). CloneAuthAWSSigV4
	// means CloneURL carries NO credential and the runner must sign it with its
	// own AWS identity — see internal/vcs.SignCloneURL. AWSRegion is the
	// repository's region; AWSRoleARN, when set, is the role the runner assumes
	// (by default via the job's OIDC token, keylessly).
	CloneAuth  string `json:"clone_auth,omitempty"`
	AWSRegion  string `json:"aws_region,omitempty"`
	AWSRoleARN string `json:"aws_role_arn,omitempty"`

	// Artifacts of the jobs this job needs — restored into the workspace
	// before the script runs (GitLab-style artifact passing).
	Dependencies []DependencyArtifact `json:"dependencies,omitempty"`

	// Services are GitLab-style sidecar containers started alongside the job and
	// reachable from the script by their alias hostname (docker: a per-job
	// network with network-aliases; kubernetes: extra containers in the same pod
	// with the alias mapped to 127.0.0.1 via hostAliases). Empty = no services,
	// and the docker executor uses the job's own Network setting. The shell
	// executor rejects any job that declares services. See docs/pipeline-dsl.md.
	Services []ServiceSpec `json:"services,omitempty"`

	// Network is the container network for the job (docker executor). Empty
	// means the executor default (bridge). A job with services is always given
	// its own per-job network instead, so the two are mutually exclusive.
	Network string `json:"network,omitempty"`
}

// CloneAuthAWSSigV4 marks a checkout that must be signed with the runner's AWS
// identity rather than cloned with a credential embedded by the control plane.
const CloneAuthAWSSigV4 = "aws-sigv4"

// ServiceSpec is one sidecar service container attached to a job.
type ServiceSpec struct {
	Image string            `json:"image"`         // container image (required)
	Alias string            `json:"alias"`         // network hostname the job reaches it by
	Env   map[string]string `json:"env,omitempty"` // environment for the service container
	Cmd   []string          `json:"cmd,omitempty"` // optional command override (docker CMD / k8s command)
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
// CodeCommit is the exception: it holds no credential at all and is reached with
// an AWS identity resolved at call time (see the AWS* fields).
type RepoRegistration struct {
	Repo          string    `json:"repo"`
	Provider      string    `json:"provider"` // github | bitbucket | codecommit | other
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

	// AWS CodeCommit (codecommit only). CodeCommit has no token — every call is
	// SigV4-signed with an AWS identity — so none of these are secret and all
	// three ARE returned by the API. AWSRegion is required (it is also encoded in
	// the derived clone URL); AWSProfile and AWSRoleARN scope the CONTROL PLANE's
	// identity. The runner authenticates separately and keylessly via per-job
	// OIDC, so nothing here is a credential.
	AWSRegion  string `json:"aws_region,omitempty"`
	AWSProfile string `json:"aws_profile,omitempty"`
	AWSRoleARN string `json:"aws_role_arn,omitempty"`

	// config-from-repo. ConfigSource is 'repo' (prefer the in-repo .forge-ci.yml
	// fetched at the event sha, fall back to the registered config) or
	// 'registered' (only ever use the registered config). Empty defaults to
	// 'repo'. ConfigPath overrides the fetched path ('' = .forge-ci.yml).
	ConfigSource string `json:"config_source,omitempty"`
	ConfigPath   string `json:"config_path,omitempty"`
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
	// Version is what the runner reported; Outdated when it differs from
	// the runner the server bundles. Draining: taking no new jobs, with
	// RunningJobs still in flight. UpdateRequested: will self-update when idle.
	Version         string `json:"version,omitempty"`
	Outdated        bool   `json:"outdated,omitempty"`
	Draining        bool   `json:"draining,omitempty"`
	UpdateRequested bool   `json:"update_requested,omitempty"`
	RunningJobs     int    `json:"running_jobs"`
	// Concurrency is the runner's job slots. Stale: not seen for a day —
	// left out of capacity and tag counts, and removable.
	Concurrency int  `json:"concurrency"`
	Stale       bool `json:"stale"`
	// Pool: the pool the runner reported, or — PoolInferred — the one pool
	// whose tags equal the runner's (how the autoscaler counts its runners).
	Pool         string `json:"pool,omitempty"`
	PoolInferred bool   `json:"pool_inferred,omitempty"`
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
	// ExpiresAt is the per-artifact expiry (from artifacts.expire_in); nil when
	// the artifact has no explicit expiry and relies on the RETENTION_DAYS sweep.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// JUnitReport is a per-job test summary parsed from uploaded JUnit XML.
type JUnitReport struct {
	JobID           int64          `json:"job_id"`
	Total           int            `json:"total"`
	Passed          int            `json:"passed"`
	Failed          int            `json:"failed"`
	Skipped         int            `json:"skipped"`
	DurationSeconds float64        `json:"duration_seconds"`
	Failures        []JUnitFailure `json:"failures"`
	CreatedAt       time.Time      `json:"created_at"`
}

// JUnitFailure names one failed or errored test case.
type JUnitFailure struct {
	Name      string `json:"name"`
	Classname string `json:"classname,omitempty"`
	Type      string `json:"type"`              // "failure" | "error"
	Message   string `json:"message,omitempty"` // trimmed failure/error message
}

// TestCaseResult is one test case's outcome from a single job's JUnit report.
// Unlike JUnitReport (an aggregate summary, overwritten on re-upload), a row is
// kept for every case on every run, so a case's pass/fail trend can be read
// back across jobs — see internal/store SaveTestCaseResults/ListTestCaseHistory.
type TestCaseResult struct {
	ID              int64     `json:"id"`
	JobID           int64     `json:"job_id"`
	Name            string    `json:"name"`
	Classname       string    `json:"classname,omitempty"`
	Status          string    `json:"status"` // passed | failed | skipped
	DurationSeconds float64   `json:"duration_seconds"`
	Message         string    `json:"message,omitempty"` // only set for failed/error cases
	CreatedAt       time.Time `json:"created_at"`
}

// TestCaseHistory is one test case's identity plus its recent runs, newest
// first — the data behind a repo's Tests tab. Runs is capped per case by the
// ?limit on GET /api/v1/test-history (see store.ListTestCaseHistory).
type TestCaseHistory struct {
	Name      string           `json:"name"`
	Classname string           `json:"classname,omitempty"`
	Runs      []TestCaseResult `json:"runs"`
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
	Version  string   `json:"version,omitempty"` // runner build version
	// Concurrency is how many jobs the runner runs in parallel (its slots);
	// older runners omit it and count as 1.
	Concurrency int `json:"concurrency,omitempty"`
	// Pool is the autoscaled pool that launched the runner (RUNNER_POOL), if any.
	Pool string `json:"pool,omitempty"`
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
	// PromotedFrom is the deployment a promotion pipeline redeploys (0 = not a
	// promotion). ExternalNeeds wires needs on jobs that aren't in this
	// pipeline to jobs of another one (the build a promotion reuses).
	PromotedFrom  int64            `json:"-"`
	ExternalNeeds map[string]int64 `json:"-"`

	Repo        string `json:"repo"`
	Ref         string `json:"ref"`
	SHA         string `json:"sha"`
	Config      string `json:"config"`
	TriggeredBy string `json:"triggered_by,omitempty"`
	// ConfigSource notes where Config came from for audit/retry ('repo' =
	// in-repo .forge-ci.yml, else 'registered'). Empty defaults to 'registered'.
	ConfigSource string `json:"config_source,omitempty"`
	// Source is the trigger (api | push | webhook | merge_request | schedule).
	// Empty defaults to 'api'.
	Source string `json:"source,omitempty"`

	// Pull-request identity for a merge_request pipeline, recorded so a provider
	// with no commit-status API can report back by commenting on the PR. MRIID is
	// the provider's pull request id (a string: CodeCommit's are); MRBaseSHA is
	// the destination-branch commit the PR targets. Both empty for push runs.
	MRIID     string `json:"mr_iid,omitempty"`
	MRBaseSHA string `json:"mr_base_sha,omitempty"`

	// CommitAuthor and CommitMessage describe the CODE, where TriggeredBy
	// describes the actor. Optional on an API run; webhooks fill them from the
	// provider event.
	CommitAuthor  string `json:"commit_author,omitempty"`
	CommitMessage string `json:"commit_message,omitempty"`
}

// Schedule is a cron-scheduled pipeline trigger for a (repo, ref). The cron
// expression is a standard 5-field spec evaluated in UTC; next_run_at is the
// server-computed next fire time and last_run_at the most recent fire (nil until
// it first fires).
type Schedule struct {
	ID        int64      `json:"id"`
	Repo      string     `json:"repo"`
	Ref       string     `json:"ref"`
	Cron      string     `json:"cron"`
	Enabled   bool       `json:"enabled"`
	CreatedBy string     `json:"created_by"`
	CreatedAt time.Time  `json:"created_at"`
	LastRunAt *time.Time `json:"last_run_at"`
	NextRunAt time.Time  `json:"next_run_at"`
}

// Member is a repo membership row (RBAC).
type Member struct {
	ID       int64  `json:"id"`
	Repo     string `json:"repo"`
	Username string `json:"username"`
	Role     string `json:"role"` // admin | owner | developer
}

// Deployment is one recorded deployment: an environment-targeting job that
// reached success. It is the atom of the ArgoCD-style environments board.
type Deployment struct {
	ID          int64     `json:"id"`
	Repo        string    `json:"repo"`
	Environment string    `json:"environment"`
	SHA         string    `json:"sha"`
	Ref         string    `json:"ref"`
	PipelineID  int64     `json:"pipeline_id"`
	JobID       int64     `json:"job_id"`
	DeployedBy  string    `json:"deployed_by"`
	DeployedAt  time.Time `json:"deployed_at"`
	Status      string    `json:"status"`
	// URL is where this deployment is reachable, Version what was deployed
	// (the job's DEPLOY_VERSION, else the short sha), Digest the immutable
	// build identity (an image digest, artifact checksum...) when the job
	// reports one. PromotedFrom is the deployment this one was promoted from.
	URL          string `json:"url,omitempty"`
	Version      string `json:"version,omitempty"`
	Digest       string `json:"digest,omitempty"`
	PromotedFrom *int64 `json:"promoted_from,omitempty"`
}

// Drift indicator values for an environment card.
const (
	DriftInSync  = "in_sync" // deployed sha == ref tip
	DriftDrifted = "drifted" // deployed sha != ref tip
	DriftUnknown = "unknown" // ref tip could not be resolved (repo not connected)
)

// EnvironmentBoard is one card on the per-repo environments board: the current
// deployment plus derived indicators.
type EnvironmentBoard struct {
	Repo            string       `json:"repo"`
	Environment     string       `json:"environment"`
	Current         *Deployment  `json:"current"`          // latest successful deployment
	DeploymentCount int          `json:"deployment_count"` // total successful deployments to this env
	Drift           string       `json:"drift"`            // in_sync | drifted | unknown
	RefTipSHA       string       `json:"ref_tip_sha,omitempty"`
	Frozen          bool         `json:"frozen"` // an active deploy freeze covers this env now
	Live            *LiveSummary `json:"live,omitempty"`
}

// LiveSummary is what Forge Live sees running for one repo × environment.
type LiveSummary struct {
	Apps   int            `json:"apps"`
	Health string         `json:"health"` // the worst of the apps'
	Counts map[string]int `json:"counts"` // apps per health
}

// RollbackRequest targets a prior deployment to re-deploy. Exactly one of the
// two fields is used: ToPipelineID takes precedence when > 0, else ToSHA.
type RollbackRequest struct {
	ToPipelineID int64  `json:"to_pipeline_id,omitempty"`
	ToSHA        string `json:"to_sha,omitempty"`
}

// DeployFreeze is a window during which deployments to an environment are held.
// Repo="" makes the freeze global (applies to every repo).
type DeployFreeze struct {
	ID          int64     `json:"id"`
	Repo        string    `json:"repo"`
	Environment string    `json:"environment"`
	StartsAt    time.Time `json:"starts_at"`
	EndsAt      time.Time `json:"ends_at"`
	Reason      string    `json:"reason"`
	CreatedAt   time.Time `json:"created_at"`
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
	// RequiredGroup / RequiredGroupApprovals: of the RequiredApprovals, at
	// least this many must come from members of this approver group.
	RequiredGroup          string `json:"required_group"`
	RequiredGroupApprovals int    `json:"required_group_approvals"`
	// Inactive: the environment registry is in use and this rule's name
	// resolves to no registered environment, so no job can ever match it.
	Inactive bool `json:"inactive,omitempty"`
}

// OIDCTokenResponse is a freshly minted per-job OIDC token (runner protocol).
type OIDCTokenResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// CatalogTemplate is one published version of an org-wide pipeline template,
// included as `template: name@version`.
type CatalogTemplate struct {
	Name        string          `json:"name"`
	Version     string          `json:"version"`
	YAML        string          `json:"yaml"`
	Description string          `json:"description"`
	PublishedBy string          `json:"published_by"`
	CreatedAt   time.Time       `json:"created_at"`
	Inputs      []TemplateInput `json:"inputs"`
}

// TemplateInput is an input a template declares under spec: inputs:.
type TemplateInput struct {
	Name        string   `json:"name"`
	Default     string   `json:"default,omitempty"`
	Required    bool     `json:"required"`
	Description string   `json:"description,omitempty"`
	Options     []string `json:"options,omitempty"`
}

// SecretSpec names one external secret for the runner to fetch. Exactly one
// provider field is set. File asks for the value to be written to a file in
// the job's run directory, the variable holding its path.
type SecretSpec struct {
	Vault string `json:"vault,omitempty"` // KV v2 "path#field" or "path#field@mount"
	AWS   string `json:"aws,omitempty"`   // Secrets Manager name or ARN, optional "#json-key"
	GCP   string `json:"gcp,omitempty"`   // "projects/P/secrets/S[/versions/V]" or "S" (needs GCP_PROJECT)
	Azure string `json:"azure,omitempty"` // "https://<vault>.vault.azure.net/secrets/<name>[/<version>]"
	File  bool   `json:"file,omitempty"`
}

// ---- runner tasks (code review, batch 7) ----

// Runner tasks are small system work that runs inside the runner process,
// not in an executor: today, keeping a repo's blobless git mirror and
// computing a diff from it. They have their own queue and long-poll, so they
// never take a job slot, and repo affinity keeps a repo's mirror on one
// runner.

// TaskKindDiff computes a diff between two commits from the repo's mirror.
const TaskKindDiff = "diff"

// TaskKindMerge merges commits onto a branch in the repo's mirror and pushes
// the result: merging a Forge-native change, or building a merge queue's
// test commit.
const TaskKindMerge = "merge"

// TaskAcquireRequest asks for a task of one of Kinds.
type TaskAcquireRequest struct {
	RunnerID string   `json:"runner_id"`
	Version  string   `json:"version,omitempty"`
	Kinds    []string `json:"kinds"`
}

// RunnerTask is a claimed task. CloneURL carries a fresh credential (minted at
// claim time); RedactValues must never be logged or written to disk.
type RunnerTask struct {
	ID           int64      `json:"id"`
	Kind         string     `json:"kind"`
	Repo         string     `json:"repo"`
	CloneURL     string     `json:"clone_url"`
	RedactValues []string   `json:"redact_values,omitempty"`
	Diff         *DiffTask  `json:"diff,omitempty"`
	Merge        *MergeTask `json:"merge,omitempty"`
}

// DiffTask: diff From..To. Mode "change" diffs from the merge base of From
// (the target branch) and To (the head) — what a pull request changed; mode
// "range" diffs the two commits directly (revision to revision). FetchRefs
// are the refs to fetch when either commit is missing from the mirror.
type DiffTask struct {
	From      string   `json:"from"`
	To        string   `json:"to"`
	Mode      string   `json:"mode"`
	FetchRefs []string `json:"fetch_refs"`
}

// DiffFile is one changed file. Patch is the unified diff for the file
// (headers included); empty for binary files and for patches over the cap.
type DiffFile struct {
	Path      string `json:"path"`
	OldPath   string `json:"old_path,omitempty"`
	Status    string `json:"status"` // added | modified | deleted | renamed | copied | type_changed
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Binary    bool   `json:"binary,omitempty"`
	Generated bool   `json:"generated,omitempty"`
	TooLarge  bool   `json:"too_large,omitempty"` // patch dropped: over the per-file cap
	Patch     string `json:"patch,omitempty"`
}

// DiffResult is what the runner reports for a diff task.
type DiffResult struct {
	MergeBase string     `json:"merge_base"` // the commit the diff starts from
	Files     []DiffFile `json:"files"`
	Truncated bool       `json:"truncated,omitempty"` // later files' patches dropped: total over the cap
	Fetched   bool       `json:"fetched"`             // the mirror had to fetch (a git call to the host)
	ComputeMS int        `json:"compute_ms"`
	// OwnersRead: the runner looked for a CODEOWNERS file at From (mode
	// "change" only; older runners never set it). CodeOwnersPath is "" when
	// there is none.
	OwnersRead     bool   `json:"owners_read,omitempty"`
	CodeOwnersPath string `json:"codeowners_path,omitempty"`
	CodeOwners     string `json:"codeowners,omitempty"`
}

// MergeTask merges Heads, in order, onto Target's tip and pushes the result
// to PushRef. ExpectedTarget (when set) and ExpectedSource (Source's tip)
// must still hold, or nothing is pushed (Moved). Without Force the push
// only lands if the remote ref still points where it was fetched (a lease),
// so a concurrent push is never overwritten.
type MergeTask struct {
	Target         string   `json:"target"`
	ExpectedTarget string   `json:"expected_target,omitempty"`
	Source         string   `json:"source,omitempty"`
	ExpectedSource string   `json:"expected_source,omitempty"`
	Heads          []string `json:"heads"`
	Method         string   `json:"method"` // merge | squash | fast_forward
	Message        string   `json:"message"`
	AuthorName     string   `json:"author_name"`
	AuthorEmail    string   `json:"author_email"`
	PushRef        string   `json:"push_ref"`
	Force          bool     `json:"force,omitempty"`
	// FetchRefs are fetched (best effort) when a head is missing, e.g. a
	// pull request's head ref on a host that serves no commit by sha.
	FetchRefs []string `json:"fetch_refs,omitempty"`
}

// MergeResult is a merge task's outcome. A conflict or a moved branch is an
// answer, not an error: retrying would not change it.
type MergeResult struct {
	SHA       string   `json:"sha,omitempty"`
	Base      string   `json:"base,omitempty"` // Target's tip the merge was made on
	Conflicts []string `json:"conflicts,omitempty"`
	Moved     string   `json:"moved,omitempty"` // why: which branch moved
	NotFF     bool     `json:"not_fast_forward,omitempty"`
}

// TaskCompleteRequest reports a task's outcome: Error set means it failed.
type TaskCompleteRequest struct {
	RunnerID string       `json:"runner_id"`
	Error    string       `json:"error,omitempty"`
	Diff     *DiffResult  `json:"diff,omitempty"`
	Merge    *MergeResult `json:"merge,omitempty"`
}
