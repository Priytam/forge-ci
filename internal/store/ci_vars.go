package store

import (
	"strconv"
	"strings"
)

// JobContext is the pipeline and job facts exposed to job scripts as
// predefined CI_* variables.
type JobContext struct {
	Repo          string
	Ref           string
	SHA           string
	Source        string
	TriggeredBy   string
	CommitAuthor  string
	CommitMessage string
	Environment   string
	PipelineID    int64
	JobID         int64
	JobName       string
}

// PredefinedVars returns the GitLab-compatible CI_* variables for a job. A
// ref of the form refs/tags/v1 is a tag (CI_COMMIT_TAG set, CI_COMMIT_BRANCH
// unset); refs/heads/x and a bare name are branches. Repo variables and the
// job's own variables override these, as in GitLab.
func PredefinedVars(c JobContext) map[string]string {
	ref := c.Ref
	v := map[string]string{
		"CI":                 "true",
		"FORGE_CI":           "true",
		"CI_COMMIT_SHA":      c.SHA,
		"CI_COMMIT_REF_NAME": ref,
		"CI_PIPELINE_ID":     strconv.FormatInt(c.PipelineID, 10),
		"CI_PIPELINE_SOURCE": c.Source,
		"CI_JOB_ID":          strconv.FormatInt(c.JobID, 10),
		"CI_JOB_NAME":        c.JobName,
		"CI_PROJECT_PATH":    c.Repo,
	}
	switch {
	case strings.HasPrefix(ref, "refs/tags/"):
		name := strings.TrimPrefix(ref, "refs/tags/")
		v["CI_COMMIT_TAG"] = name
		v["CI_COMMIT_REF_NAME"] = name
	default:
		name := strings.TrimPrefix(ref, "refs/heads/")
		v["CI_COMMIT_BRANCH"] = name
		v["CI_COMMIT_REF_NAME"] = name
	}
	v["CI_COMMIT_REF_SLUG"] = refSlug(v["CI_COMMIT_REF_NAME"])
	if len(c.SHA) >= 8 {
		v["CI_COMMIT_SHORT_SHA"] = c.SHA[:8]
	} else {
		v["CI_COMMIT_SHORT_SHA"] = c.SHA
	}
	if i := strings.LastIndex(c.Repo, "/"); i >= 0 {
		v["CI_PROJECT_NAMESPACE"] = c.Repo[:i]
		v["CI_PROJECT_NAME"] = c.Repo[i+1:]
	} else {
		v["CI_PROJECT_NAME"] = c.Repo
	}
	if c.CommitMessage != "" {
		v["CI_COMMIT_MESSAGE"] = c.CommitMessage
		v["CI_COMMIT_TITLE"] = strings.SplitN(c.CommitMessage, "\n", 2)[0]
	}
	if c.CommitAuthor != "" {
		v["CI_COMMIT_AUTHOR"] = c.CommitAuthor
	}
	if c.TriggeredBy != "" {
		v["GITLAB_USER_LOGIN"] = c.TriggeredBy
		v["CI_TRIGGERED_BY"] = c.TriggeredBy
	}
	if c.Environment != "" {
		v["CI_ENVIRONMENT_NAME"] = c.Environment
	}
	return v
}

// refSlug lowercases a ref and replaces anything but [a-z0-9] with '-',
// trimmed to 63 characters (GitLab's CI_COMMIT_REF_SLUG), for use in
// hostnames, image tags and namespaces.
func refSlug(ref string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(ref) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	s := b.String()
	if len(s) > 63 {
		s = s[:63]
	}
	return strings.Trim(s, "-")
}
