package vcs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// GitLab's change source and writers (API v4). A refresh is three calls —
// the merge request, its approvals, its discussions — because GitLab's REST
// API has no single call for all three. GitLab answers no conditional
// requests on these, so its reconcile costs one call per repo.

func (gitlabProvider) Capabilities() Capabilities {
	return Capabilities{Changes: true, Threads: true, Suggestions: true, Resolve: true, Drafts: true,
		MergeMethods: []string{"merge", "squash"}, StatusChecks: true, Reviews: true}
}

// MirrorRefs: the target branch and GitLab's merge-requests/<iid>/head.
func (gitlabProvider) MirrorRefs(id, _, target string) []string {
	return []string{headRef(target), "+refs/merge-requests/" + id + "/head:refs/merge-requests/" + id + "/head"}
}

func glBase(repo RepoRef) string {
	return gitlabAPI(repo.CloneURL) + "/projects/" + gitlabProject(repo.Repo)
}

type glMR struct {
	IID            int       `json:"iid"`
	Title          string    `json:"title"`
	Description    string    `json:"description"`
	State          string    `json:"state"` // opened | closed | merged | locked
	Draft          bool      `json:"draft"`
	WorkInProgress bool      `json:"work_in_progress"`
	SourceBranch   string    `json:"source_branch"`
	TargetBranch   string    `json:"target_branch"`
	SHA            string    `json:"sha"`
	WebURL         string    `json:"web_url"`
	Labels         []string  `json:"labels"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	Author         struct {
		Username string `json:"username"`
	} `json:"author"`
	DiffRefs *struct {
		BaseSHA  string `json:"base_sha"`
		HeadSHA  string `json:"head_sha"`
		StartSHA string `json:"start_sha"`
	} `json:"diff_refs"`
}

type glNote struct {
	ID        int64     `json:"id"`
	Body      string    `json:"body"`
	System    bool      `json:"system"`
	Resolved  bool      `json:"resolved"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Author    struct {
		Username string `json:"username"`
	} `json:"author"`
	Position *struct {
		NewPath string `json:"new_path"`
		OldPath string `json:"old_path"`
		NewLine int    `json:"new_line"`
		OldLine int    `json:"old_line"`
		HeadSHA string `json:"head_sha"`
	} `json:"position"`
}

func (n glNote) comment() Comment {
	return Comment{ExternalID: strconv.FormatInt(n.ID, 10), Author: n.Author.Username, Body: n.Body,
		CreatedAt: n.CreatedAt, UpdatedAt: n.UpdatedAt}
}

func (gitlabProvider) NormalizeEvent(hdr http.Header, body []byte) ([]ChangeEvent, error) {
	var payload struct {
		ObjectAttributes struct {
			IID          int    `json:"iid"`
			Action       string `json:"action"`
			NoteableType string `json:"noteable_type"`
		} `json:"object_attributes"`
		MergeRequest *struct {
			IID int `json:"iid"`
		} `json:"merge_request"`
		Project struct {
			Path string `json:"path_with_namespace"`
		} `json:"project"`
	}
	var kind string
	switch hdr.Get("X-Gitlab-Event") {
	case "Merge Request Hook", "Note Hook":
	default:
		return nil, nil
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	iid := payload.ObjectAttributes.IID
	switch hdr.Get("X-Gitlab-Event") {
	case "Merge Request Hook":
		kind = "change"
		if a := payload.ObjectAttributes.Action; a == "approved" || a == "unapproved" || a == "approval" || a == "unapproval" {
			kind = "review"
		}
	default:
		if payload.ObjectAttributes.NoteableType != "MergeRequest" || payload.MergeRequest == nil {
			return nil, nil
		}
		kind, iid = "comment", payload.MergeRequest.IID
	}
	if iid == 0 || payload.Project.Path == "" {
		return nil, nil
	}
	return []ChangeEvent{{Repo: payload.Project.Path, ChangeID: strconv.Itoa(iid), Kind: kind,
		Action: payload.ObjectAttributes.Action, DeliveryID: hdr.Get("X-Gitlab-Event-UUID")}}, nil
}

func (gitlabProvider) GetChange(ctx context.Context, c *ChangeClient, repo RepoRef, id string, _ *Change) (*Change, error) {
	base, auth := glBase(repo)+"/merge_requests/"+url.PathEscape(id), bearer(repo.Token)
	var mr glMR
	if err := c.jsonCall(ctx, "gitlab", http.MethodGet, base, auth, nil, &mr); err != nil {
		return nil, err
	}
	var approvals struct {
		ApprovedBy []struct {
			User struct {
				Username string `json:"username"`
			} `json:"user"`
		} `json:"approved_by"`
	}
	if err := c.jsonCall(ctx, "gitlab", http.MethodGet, base+"/approvals", auth, nil, &approvals); err != nil {
		return nil, err
	}
	var discussions []struct {
		ID    string   `json:"id"`
		Notes []glNote `json:"notes"`
	}
	if err := c.jsonCall(ctx, "gitlab", http.MethodGet, base+"/discussions?per_page=100", auth, nil, &discussions); err != nil {
		return nil, err
	}
	state := map[string]string{"opened": "open", "merged": "merged"}[mr.State]
	if state == "" {
		state = "closed"
	}
	ch := &Change{ID: strconv.Itoa(mr.IID), Title: mr.Title, Body: mr.Description, Author: mr.Author.Username,
		SourceBranch: mr.SourceBranch, TargetBranch: mr.TargetBranch, State: state, Draft: mr.Draft || mr.WorkInProgress,
		HeadSHA: mr.SHA, Labels: append([]string{}, mr.Labels...), URL: mr.WebURL, CreatedAt: mr.CreatedAt,
		UpdatedAt: mr.UpdatedAt, Reviews: []Review{}, Threads: []Thread{}}
	if mr.DiffRefs != nil {
		ch.BaseSHA = mr.DiffRefs.StartSHA // the target tip the diff is against
	}
	for _, a := range approvals.ApprovedBy {
		ch.Reviews = append(ch.Reviews, Review{ExternalID: "approval:" + a.User.Username, Reviewer: a.User.Username,
			Verdict: "approve", SubmittedAt: mr.UpdatedAt})
	}
	conv := Thread{ExternalID: ConversationThread, Comments: []Comment{}}
	for _, d := range discussions {
		var notes []glNote
		for _, n := range d.Notes {
			if !n.System {
				notes = append(notes, n)
			}
		}
		if len(notes) == 0 {
			continue
		}
		pos := notes[0].Position
		if pos == nil {
			for _, n := range notes {
				conv.Comments = append(conv.Comments, n.comment())
			}
			continue
		}
		th := Thread{ExternalID: d.ID, Path: pos.NewPath, Side: "new", Line: pos.NewLine, OriginalLine: pos.NewLine,
			CommitSHA: pos.HeadSHA, OriginalCommit: pos.HeadSHA, Resolved: notes[0].Resolved, Outdated: pos.HeadSHA != mr.SHA}
		if pos.NewLine == 0 {
			th.Path, th.Side, th.Line, th.OriginalLine = pos.OldPath, "old", pos.OldLine, pos.OldLine
		}
		if th.Outdated {
			th.Line = 0
		}
		for _, n := range notes {
			th.Comments = append(th.Comments, n.comment())
		}
		ch.Threads = append(ch.Threads, th)
	}
	ch.Threads = append(ch.Threads, conv)
	return ch, nil
}

func (gitlabProvider) ListOpenChanges(ctx context.Context, c *ChangeClient, repo RepoRef, v Validators) ([]ChangeRef, Validators, bool, error) {
	next := glBase(repo) + "/merge_requests?state=opened&per_page=100&order_by=updated_at"
	var refs []ChangeRef
	for page := 0; next != ""; page++ {
		if page == maxListPages {
			return refs, v, false, nil
		}
		req, err := http.NewRequest(http.MethodGet, next, nil)
		if err != nil {
			return nil, v, false, err
		}
		for k, val := range bearer(repo.Token) {
			req.Header.Set(k, val)
		}
		resp, err := c.do(ctx, "gitlab", req)
		if err != nil {
			return nil, v, false, err
		}
		var mrs []glMR
		err = json.NewDecoder(resp.Body).Decode(&mrs)
		resp.Body.Close()
		if err != nil {
			return nil, v, false, fmt.Errorf("vcs: decode gitlab merge requests: %w", err)
		}
		for _, mr := range mrs {
			refs = append(refs, ChangeRef{ID: strconv.Itoa(mr.IID), HeadSHA: mr.SHA, UpdatedAt: mr.UpdatedAt})
		}
		next = nextLink(resp.Header)
	}
	return refs, v, true, nil
}

// PublishCheck sets a commit status. With "Pipelines must succeed" on the
// project, a pending or failed forge/merge-gate keeps the merge button off.
func (gitlabProvider) PublishCheck(ctx context.Context, c *ChangeClient, repo RepoRef, sha string, res CheckResult) error {
	state := map[string]string{"success": "success", "failure": "failed"}[res.State]
	if state == "" {
		state = "pending"
	}
	return c.jsonCall(ctx, "gitlab", http.MethodPost, glBase(repo)+"/statuses/"+url.PathEscape(sha), bearer(repo.Token),
		map[string]string{"state": state, "name": GateContext, "description": res.Description, "target_url": res.TargetURL}, nil)
}

// Merge merges only if the head is still ExpectedHead (GitLab answers 409).
func (gitlabProvider) Merge(ctx context.Context, c *ChangeClient, repo RepoRef, id string, opts MergeOpts) (string, error) {
	var out struct {
		MergeCommitSHA string `json:"merge_commit_sha"`
		State          string `json:"state"`
	}
	err := c.jsonCall(ctx, "gitlab", http.MethodPut, glBase(repo)+"/merge_requests/"+url.PathEscape(id)+"/merge",
		bearer(repo.Token), map[string]any{"sha": opts.ExpectedHead, "squash": opts.Method == "squash"}, &out)
	var se *StatusError
	switch {
	case errors.As(err, &se) && se.StatusCode == http.StatusConflict:
		return "", fmt.Errorf("%w (%s)", ErrHeadMoved, se.Message)
	case errors.As(err, &se) && (se.StatusCode == http.StatusMethodNotAllowed || se.StatusCode == http.StatusNotAcceptable ||
		se.StatusCode == http.StatusUnprocessableEntity):
		return "", fmt.Errorf("%w: %s", ErrNotMergeable, se.Message)
	case err != nil:
		return "", err
	}
	return out.MergeCommitSHA, nil
}

// PostComment: a reply goes to its discussion, a new line thread is a
// positioned discussion on the merge request's current diff, anything else
// is a note on the merge request.
func (gitlabProvider) PostComment(ctx context.Context, c *ChangeClient, repo RepoRef, id string, req CommentRequest) (string, error) {
	base, auth := glBase(repo)+"/merge_requests/"+url.PathEscape(id), bearer(repo.Token)
	var out struct {
		ID    any `json:"id"`
		Notes []struct {
			ID int64 `json:"id"`
		} `json:"notes"`
	}
	switch {
	case req.ThreadID != "":
		err := c.jsonCall(ctx, "gitlab", http.MethodPost, base+"/discussions/"+url.PathEscape(req.ThreadID)+"/notes", auth,
			map[string]string{"body": req.Body}, &out)
		if err != nil {
			return "", err
		}
		return fmt.Sprint(out.ID), nil
	case req.Anchor != nil:
		var mr glMR
		if err := c.jsonCall(ctx, "gitlab", http.MethodGet, base, auth, nil, &mr); err != nil {
			return "", err
		}
		if mr.DiffRefs == nil {
			return "", errors.New("vcs: gitlab merge request has no diff yet")
		}
		pos := map[string]any{"position_type": "text", "base_sha": mr.DiffRefs.BaseSHA, "start_sha": mr.DiffRefs.StartSHA,
			"head_sha": req.Anchor.CommitSHA, "new_path": req.Anchor.Path, "old_path": req.Anchor.Path}
		if req.Anchor.Side == "old" {
			pos["old_line"] = req.Anchor.Line
		} else {
			pos["new_line"] = req.Anchor.Line
		}
		if err := c.jsonCall(ctx, "gitlab", http.MethodPost, base+"/discussions", auth,
			map[string]any{"body": req.Body, "position": pos}, &out); err != nil {
			return "", err
		}
		if len(out.Notes) == 0 {
			return "", errors.New("vcs: gitlab created a discussion without a note")
		}
		return strconv.FormatInt(out.Notes[0].ID, 10), nil
	default:
		if err := c.jsonCall(ctx, "gitlab", http.MethodPost, base+"/notes", auth, map[string]string{"body": req.Body}, &out); err != nil {
			return "", err
		}
		return fmt.Sprint(out.ID), nil
	}
}

func (gitlabProvider) ResolveThread(ctx context.Context, c *ChangeClient, repo RepoRef, id, threadID string, resolved bool) error {
	u := fmt.Sprintf("%s/merge_requests/%s/discussions/%s?resolved=%t", glBase(repo), url.PathEscape(id), url.PathEscape(threadID), resolved)
	return c.jsonCall(ctx, "gitlab", http.MethodPut, u, bearer(repo.Token), nil, nil)
}

// PostReview: approve is GitLab's approval (on the given head); a request
// for changes or a comment is a note (GitLab Free has no "request changes").
func (gitlabProvider) PostReview(ctx context.Context, c *ChangeClient, repo RepoRef, id, verdict, body, commitSHA string) (string, error) {
	base, auth := glBase(repo)+"/merge_requests/"+url.PathEscape(id), bearer(repo.Token)
	switch verdict {
	case "approve":
		if err := c.jsonCall(ctx, "gitlab", http.MethodPost, base+"/approve", auth, map[string]string{"sha": commitSHA}, nil); err != nil {
			return "", err
		}
		return "approval", nil
	case "request_changes", "comment":
		text := body
		if verdict == "request_changes" {
			text = strings.TrimSpace("**Changes requested.** " + body)
		}
		var out struct {
			ID int64 `json:"id"`
		}
		if err := c.jsonCall(ctx, "gitlab", http.MethodPost, base+"/notes", auth, map[string]string{"body": text}, &out); err != nil {
			return "", err
		}
		return strconv.FormatInt(out.ID, 10), nil
	}
	return "", fmt.Errorf("unknown verdict %q", verdict)
}
