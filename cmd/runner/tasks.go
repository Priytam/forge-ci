package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/buildinfo"
	"github.com/priytamjeepandey/forge-ci/internal/mirror"
	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

// Runner tasks: small system work the runner does itself, outside the job
// slots and the executor — diffs for code review, and merges (Forge-native
// changes, merge queue test commits), from a blobless git mirror kept on
// this machine (RUNNER_MIRROR_DIR). The server sends a repo's tasks to the
// runner that already holds its mirror. RUNNER_TASKS=off opts out.

// oldServerPause: a server without the task protocol answers 404; ask again
// rarely rather than every poll.
const oldServerPause = 10 * time.Minute

func taskLoop(ctx context.Context, c *client, runnerID string) {
	if os.Getenv("RUNNER_TASKS") == "off" {
		slog.Info("runner tasks: off (RUNNER_TASKS=off)")
		return
	}
	if !mirror.Supported() {
		slog.Info("runner tasks: git 2.27+ not found; this runner will not compute diffs")
		return
	}
	root := mirror.Root()
	slog.Info("runner tasks: on", "mirror_dir", root)
	upload := &client{base: c.base, http: &http.Client{Timeout: 5 * time.Minute}, token: c.token}
	kinds := []string{proto.TaskKindDiff}
	if mirror.MergeSupported() {
		kinds = append(kinds, proto.TaskKindMerge)
	} else {
		slog.Info("runner tasks: git 2.38+ not found; this runner will not merge")
	}
	req := proto.TaskAcquireRequest{RunnerID: runnerID, Version: buildinfo.Version, Kinds: kinds}
	for ctx.Err() == nil {
		task, status, err := acquireTask(ctx, c, req)
		switch {
		case status == http.StatusNotFound:
			logOnce("tasks-404", slog.LevelInfo, "runner tasks: the server does not offer tasks yet")
			sleep(ctx, oldServerPause)
			continue
		case err != nil:
			if ctx.Err() == nil {
				slog.Warn("runner tasks: acquire failed, retrying", "err", err)
				sleep(ctx, acquireRetryPause)
			}
			continue
		case task == nil:
			continue
		}
		report := runTask(ctx, task, root)
		report.RunnerID = runnerID
		completeTask(ctx, upload, task.ID, report)
	}
}

func acquireTask(ctx context.Context, c *client, req proto.TaskAcquireRequest) (*proto.RunnerTask, int, error) {
	resp, err := c.postJSON(ctx, "/api/v1/runner/tasks/acquire", req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent:
		return nil, resp.StatusCode, nil
	case http.StatusOK:
		var t proto.RunnerTask
		if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
			return nil, resp.StatusCode, err
		}
		return &t, resp.StatusCode, nil
	default:
		return nil, resp.StatusCode, fmt.Errorf("task acquire: unexpected status %d", resp.StatusCode)
	}
}

// runTask does one task and reports its outcome (never the credential).
func runTask(ctx context.Context, t *proto.RunnerTask, root string) proto.TaskCompleteRequest {
	failed := func(what string, err error) proto.TaskCompleteRequest {
		msg := err.Error()
		for _, v := range t.RedactValues {
			if v != "" {
				msg = strings.ReplaceAll(msg, v, "[REDACTED]")
			}
		}
		slog.Warn("runner tasks: "+what+" failed", "task", t.ID, "repo", t.Repo, "err", msg)
		return proto.TaskCompleteRequest{Error: msg}
	}
	m := mirror.Open(root, t.Repo)
	switch {
	case t.Kind == proto.TaskKindDiff && t.Diff != nil:
		res, err := m.Diff(ctx, t.CloneURL, t.RedactValues, *t.Diff)
		if err != nil {
			return failed("diff", err)
		}
		slog.Info("runner tasks: diff computed", "task", t.ID, "repo", t.Repo, "files", len(res.Files),
			"fetched", res.Fetched, "ms", res.ComputeMS)
		return proto.TaskCompleteRequest{Diff: res}
	case t.Kind == proto.TaskKindMerge && t.Merge != nil:
		res, err := m.Merge(ctx, t.CloneURL, t.RedactValues, *t.Merge)
		if err != nil {
			return failed("merge", err)
		}
		slog.Info("runner tasks: merge done", "task", t.ID, "repo", t.Repo, "pushed", res.SHA,
			"conflicts", len(res.Conflicts), "moved", res.Moved)
		return proto.TaskCompleteRequest{Merge: res}
	}
	return proto.TaskCompleteRequest{Error: "this runner cannot do task kind " + t.Kind}
}

// completeTask reports, retrying briefly: the server keeps the lease for a
// few minutes, after which another runner redoes the task anyway.
func completeTask(ctx context.Context, c *client, id int64, report proto.TaskCompleteRequest) {
	path := fmt.Sprintf("/api/v1/runner/tasks/%d/complete", id)
	for attempt := 0; attempt < 3 && ctx.Err() == nil; attempt++ {
		resp, err := c.postJSON(context.WithoutCancel(ctx), path, report)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode < 500 {
				if resp.StatusCode >= 300 {
					slog.Warn("runner tasks: report refused", "task", id, "status", resp.StatusCode)
				}
				return
			}
		}
		sleep(ctx, acquireRetryPause)
	}
}
