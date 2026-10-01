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
	// Manual is true for a when:manual gate (released via POST /jobs/{id}/play),
	// distinguishing it from an environment-approval block. AllowFailure is true
	// when the job's failure does not fail dependents or the pipeline.
	Manual       bool `json:"manual"`
	AllowFailure bool `json:"allow_failure"`

	// Approval gate, populated only for a job that targets a protected
	// environment. Approvals is the append-only vote record (one per approver,
	// oldest first) and RequiredApprovals the count the gate needs, so a
	// reviewer can see where the gate stands before voting. Both are zero for a
	// job with no gate.
	Approvals         []JobApproval `json:"approvals,omitempty"`
	RequiredApprovals int           `json:"required_approvals,omitempty"`
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
	Repo            string      `json:"repo"`
	Environment     string      `json:"environment"`
	Current         *Deployment `json:"current"`          // latest successful deployment
	DeploymentCount int         `json:"deployment_count"` // total successful deployments to this env
	Drift           string      `json:"drift"`            // in_sync | drifted | unknown
	RefTipSHA       string      `json:"ref_tip_sha,omitempty"`
	Frozen          bool        `json:"frozen"` // an active deploy freeze covers this env now
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
}
