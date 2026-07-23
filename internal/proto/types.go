// Package proto holds the JSON types shared by the server API, the runner,
// and (structurally) the web frontend.
package proto

import "time"

type Pipeline struct {
	ID        int64     `json:"id"`
	Repo      string    `json:"repo"`
	Ref       string    `json:"ref"`
	SHA       string    `json:"sha"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
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
}

type AcquireRequest struct {
	RunnerID string `json:"runner_id"`
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
	Repo   string `json:"repo"`
	Ref    string `json:"ref"`
	SHA    string `json:"sha"`
	Config string `json:"config"`
}
