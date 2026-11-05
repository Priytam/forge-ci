package notify

import (
	"fmt"
	"strings"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/store"
)

func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

func branch(ref string) string { return strings.TrimPrefix(ref, "refs/heads/") }

// ApprovalEvent renders "a job is waiting for approval".
func ApprovalEvent(n store.ApprovalNotice, frontend string) Event {
	m := ApprovalMessage(n, frontend)
	fields := map[string]string{"Branch": branch(n.Ref) + " @ " + shortSHA(n.SHA), "Environment": n.Environment}
	if n.TriggeredBy != "" {
		fields["Started by"] = n.TriggeredBy
	}
	if len(n.Approvers) > 0 {
		fields["Can approve"] = strings.Join(n.Approvers, ", ")
	}
	return Event{
		Kind: EventApprovalNeeded, Repo: n.Repo, Ref: n.Ref, SHA: n.SHA, PipelineID: n.PipelineID,
		JobID: n.JobID, JobName: n.JobName, Environment: n.Environment, Status: "blocked",
		Title: fmt.Sprintf("%s: %s waits for approval to deploy to %s", n.Repo, n.JobName, n.Environment),
		Text:  m.Text, Link: m.Link, Fields: fields, At: time.Now(), Approvers: n.Approvers,
	}
}

// PipelineEvent renders a finished pipeline.
func PipelineEvent(p store.PipelineEventNotice, frontend string) Event {
	link := fmt.Sprintf("%s/pipelines/%d", strings.TrimSuffix(frontend, "/"), p.PipelineID)
	var verb string
	switch p.Kind {
	case EventPipelineFailed:
		verb = "failed"
	case EventPipelineFixed:
		verb = "is fixed"
	default:
		verb = "succeeded"
	}
	title := fmt.Sprintf("%s: pipeline #%d on %s %s", p.Repo, p.PipelineID, branch(p.Ref), verb)
	var b strings.Builder
	fmt.Fprintf(&b, "Pipeline #%d for %s on %s @ %s %s.", p.PipelineID, p.Repo, branch(p.Ref), shortSHA(p.SHA), verb)
	if t := strings.SplitN(p.CommitMessage, "\n", 2)[0]; t != "" {
		fmt.Fprintf(&b, "\n%s", t)
	}
	fields := map[string]string{"Branch": branch(p.Ref) + " @ " + shortSHA(p.SHA)}
	if p.TriggeredBy != "" {
		fields["Started by"] = p.TriggeredBy
	}
	if len(p.FailedJobs) > 0 {
		fields["Failed jobs"] = strings.Join(p.FailedJobs, ", ")
	}
	if p.Duration > 0 {
		fields["Duration"] = p.Duration.Round(time.Second).String()
	}
	return Event{
		Kind: p.Kind, Repo: p.Repo, Ref: p.Ref, SHA: p.SHA, PipelineID: p.PipelineID, Status: p.Status,
		Title: title, Text: b.String(), Link: link, Fields: fields, At: time.Now(),
	}
}

// DeployEvent renders a finished deploy job.
func DeployEvent(d store.DeployEventNotice, frontend string) Event {
	link := fmt.Sprintf("%s/jobs/%d?pipeline=%d", strings.TrimSuffix(frontend, "/"), d.JobID, d.PipelineID)
	verb := "deployed"
	if d.Kind == EventDeployFailed {
		verb = "failed to deploy"
	}
	what := d.Version
	if what == "" {
		what = shortSHA(d.SHA)
	}
	title := fmt.Sprintf("%s: %s %s to %s", d.Repo, what, verb, d.Environment)
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s %s to %s (job %s, pipeline #%d).", d.Repo, what, verb, d.Environment, d.JobName, d.PipelineID)
	fields := map[string]string{"Environment": d.Environment, "Branch": branch(d.Ref) + " @ " + shortSHA(d.SHA)}
	if d.DeployedBy != "" {
		fields["By"] = d.DeployedBy
	}
	if d.URL != "" {
		fields["URL"] = d.URL
	}
	if d.Digest != "" {
		fields["Digest"] = d.Digest
	}
	return Event{
		Kind: d.Kind, Repo: d.Repo, Ref: d.Ref, SHA: d.SHA, PipelineID: d.PipelineID, JobID: d.JobID,
		JobName: d.JobName, Environment: d.Environment, Status: d.Status,
		Title: title, Text: b.String(), Link: link, Fields: fields, At: time.Now(),
	}
}
