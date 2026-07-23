// Package proto holds the JSON types shared by the server API, the runner,
// and (structurally) the web frontend.
package proto

import "time"

type Pipeline struct {
	ID        int64         `json:"id"`
	Repo      string        `json:"repo"`
	Ref       string        `json:"ref"`
	SHA       string        `json:"sha"`
	Status    string        `json:"status"`
	Stages    []StageStatus `json:"stages"`
	CreatedAt time.Time     `json:"created_at"`
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
	Refs           []string  `json:"refs"`             // distinct refs, most recent first
	RecentStatuses []string  `json:"recent_statuses"`  // last pipelines' statuses, newest first
	LastPipeline   *Pipeline `json:"last_pipeline"`    // includes stages
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
	Status   string `json:"status"` // success | failed
	ExitCode int    `json:"exit_code"`
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
