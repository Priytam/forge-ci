package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/store"
)

// maxBulkRetry caps the failed jobs one bulk retry looks at.
const maxBulkRetry = 200

// bulkRetryLogBytes is how much of each job's log (its end) the error text
// is searched in.
const bulkRetryLogBytes = 256 << 10

type bulkRetrySkip struct {
	JobID  int64  `json:"job_id"`
	Reason string `json:"reason"`
}

type bulkRetryDone struct {
	JobID      int64 `json:"job_id"`
	NewJobID   int64 `json:"new_job_id"`
	PipelineID int64 `json:"pipeline_id"`
	Requeued   int   `json:"requeued"` // the job plus its dependents re-queued with it
}

// retryFailedJobs is POST /api/v1/jobs/retry-failed: after an outage, retry
// every failed job matching {name, error} that finished since a time
// (optionally in one repo). error is matched, case-insensitively, against
// the job's status reason and the end of its log. dry_run lists the jobs
// that would be retried. Each job needs the same rights as a single retry
// (a role on its repo); others are skipped with the reason. Each retry is
// audited like a single one, plus one job.retry.bulk entry.
func (s *Server) retryFailedJobs(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Repo   string `json:"repo"`
		Name   string `json:"name"`
		Error  string `json:"error"`
		Since  string `json:"since"` // RFC 3339 time, or a duration back from now ("2h")
		DryRun bool   `json:"dry_run"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	since, err := parseSince(req.Since, time.Now())
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	req.Name, req.Error = strings.TrimSpace(req.Name), strings.TrimSpace(req.Error)
	if req.Name == "" && req.Error == "" {
		writeErr(w, http.StatusBadRequest, "give name or error (or both) to say which failed jobs to retry")
		return
	}
	candidates, err := s.store.FailedJobs(r.Context(), store.FailedJobQuery{
		Repo: strings.TrimSpace(req.Repo), Name: req.Name, Since: since, Limit: maxBulkRetry + 1})
	if err != nil {
		slog.Error("bulk retry: list", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to list failed jobs")
		return
	}
	if len(candidates) > maxBulkRetry {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("more than %d failed jobs match; narrow name, error, repo or since", maxBulkRetry))
		return
	}
	matched := []store.FailedJob{}
	for _, f := range candidates {
		if req.Error == "" || s.jobFailureMentions(r, f, req.Error) {
			matched = append(matched, f)
		}
	}
	retried, skipped := []bulkRetryDone{}, []bulkRetrySkip{}
	for _, f := range matched {
		if !s.canOperate(r, f.Repo) {
			skipped = append(skipped, bulkRetrySkip{f.JobID, "you need a role on " + f.Repo})
			continue
		}
		if req.DryRun {
			continue
		}
		res, err := s.store.RetryJobWith(r.Context(), f.JobID, store.RetryOpts{})
		switch {
		case errors.Is(err, store.ErrNotRetryable):
			// Usually re-queued already, as a dependent of an earlier match.
			skipped = append(skipped, bulkRetrySkip{f.JobID, "already retried (with an earlier job of its pipeline)"})
			continue
		case errors.Is(err, store.ErrRetryRefused):
			skipped = append(skipped, bulkRetrySkip{f.JobID, err.Error()})
			continue
		case err != nil:
			slog.Error("bulk retry", "job", f.JobID, "err", err)
			skipped = append(skipped, bulkRetrySkip{f.JobID, "failed to retry"})
			continue
		}
		s.audit(r, "job.retry", strconv.FormatInt(f.JobID, 10), f.Repo, "ok", map[string]any{
			"pipeline_id": res.PipelineID, "new_job_id": res.Retried[f.JobID], "jobs_requeued": len(res.Retried), "bulk": true,
		})
		retried = append(retried, bulkRetryDone{JobID: f.JobID, NewJobID: res.Retried[f.JobID], PipelineID: res.PipelineID, Requeued: len(res.Retried)})
	}
	if !req.DryRun {
		s.audit(r, "job.retry.bulk", fmt.Sprintf("%d jobs", len(retried)), req.Repo, "ok", map[string]any{
			"name": req.Name, "error": req.Error, "since": since.Format(time.RFC3339),
			"matched": len(matched), "retried": len(retried), "skipped": len(skipped),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"dry_run": req.DryRun, "since": since, "matched": matched, "retried": retried, "skipped": skipped,
	})
}

// jobFailureMentions reports whether a failed job's status reason or the end
// of its log contains text (case-insensitive).
func (s *Server) jobFailureMentions(r *http.Request, f store.FailedJob, text string) bool {
	text = strings.ToLower(text)
	if strings.Contains(strings.ToLower(f.Reason), text) {
		return true
	}
	if s.logs == nil {
		return false
	}
	log, err := s.logs.FullText(r.Context(), f.JobID)
	if err != nil {
		return false
	}
	if len(log) > bulkRetryLogBytes {
		log = log[len(log)-bulkRetryLogBytes:]
	}
	return strings.Contains(strings.ToLower(log), text)
}

// parseSince reads an RFC 3339 time or a duration back from now. It is
// required: a bulk retry is always about a recent window.
func parseSince(v string, now time.Time) (time.Time, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, errors.New(`since is required: an RFC 3339 time or a duration such as "2h"`)
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return now.Add(-d), nil
	}
	return time.Time{}, fmt.Errorf(`since %q: want an RFC 3339 time or a duration such as "2h"`, v)
}
