package vcs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"
)

// Gitea / Forgejo change source and writers (API v1). Line comments live in
// reviews, so a refresh reads the pull request, its reviews, the comments of
// reviews that have any, and the conversation. Gitea has no API to resolve a
// conversation or answer 304s: Forge keeps the resolved flag itself, and the
// reconcile costs one call per repo.

func (giteaProvider) Capabilities() Capabilities {
	return Capabilities{Changes: true, Threads: true, MergeMethods: []string{"merge", "squash", "rebase"},
		StatusChecks: true, Reviews: true}
}

// MirrorRefs: the target branch and Gitea's pull/<n>/head.
func (giteaProvider) MirrorRefs(id, _, target string) []string {
	return []string{headRef(target), "+refs/pull/" + id + "/head:refs/pull/" + id + "/head"}
}

func gtBase(repo RepoRef) (string, error) {
	owner, name, ok := splitRepo(projectPath(repo.Repo))
	if !ok {
		return "", fmt.Errorf("invalid repo %q (want owner/name)", repo.Repo)
	}
	return giteaAPI(repo.CloneURL) + "/repos/" + owner + "/" + name, nil
}

func giteaEvent(hdr http.Header) string {
	for _, h := range []string{"X-Gitea-Event", "X-Forgejo-Event", "X-Gogs-Event"} {
		if v := hdr.Get(h); v != "" {
			return v
		}
	}
	return ""
}

func (giteaProvider) NormalizeEvent(hdr http.Header, body []byte) ([]ChangeEvent, error) {
	var kind string
	switch ev := giteaEvent(hdr); ev {
	case "pull_request", "pull_request_sync", "pull_request_label", "pull_request_milestone", "pull_request_assign":
		kind = "change"
	case "pull_request_review_approved", "pull_request_review_rejected", "pull_request_review_comment", "pull_request_review":
		kind = "review"
	case "pull_request_comment", "issue_comment":
		kind = "comment"
	default:
		return nil, nil
	}
	var payload struct {
		Action      string  `json:"action"`
		PullRequest *ghPull `json:"pull_request"`
		Issue       *struct {
			Number      int       `json:"number"`
			PullRequest *struct{} `json:"pull_request"`
		} `json:"issue"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	number := 0
	switch {
	case payload.PullRequest != nil:
		number = payload.PullRequest.Number
	case payload.Issue != nil && payload.Issue.PullRequest != nil:
		number = payload.Issue.Number
	}
	if number == 0 || payload.Repository.FullName == "" {
		return nil, nil
	}
	ev := ChangeEvent{Repo: payload.Repository.FullName, ChangeID: strconv.Itoa(number), Kind: kind,
		Action: payload.Action, DeliveryID: hdr.Get("X-Gitea-Delivery")}
	if kind == "change" && payload.PullRequest != nil && payload.PullRequest.Head.SHA != "" {
		ev.Snapshot = payload.PullRequest.change() // same shape as GitHub's
	}
	return []ChangeEvent{ev}, nil
}

type gtComment struct {
	ID               int64     `json:"id"`
	Body             string    `json:"body"`
	Path             string    `json:"path"`
	Position         int       `json:"position"`          // line on the new side
	OriginalPosition int       `json:"original_position"` // line on the old side
	CommitID         string    `json:"commit_id"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
	User             struct {
		Login string `json:"login"`
	} `json:"user"`
}

func (c gtComment) comment() Comment {
	return Comment{ExternalID: strconv.FormatInt(c.ID, 10), Author: c.User.Login, Body: c.Body, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt}
}

func (giteaProvider) GetChange(ctx context.Context, c *ChangeClient, repo RepoRef, id string, _ *Change) (*Change, error) {
	base, err := gtBase(repo)
	if err != nil {
		return nil, err
	}
	auth := giteaAuth(repo.Token)
	var pr ghPull
	if err := c.jsonCall(ctx, "gitea", http.MethodGet, base+"/pulls/"+id, auth, nil, &pr); err != nil {
		return nil, err
	}
	var reviews []struct {
		ID            int64     `json:"id"`
		State         string    `json:"state"`
		Body          string    `json:"body"`
		CommitID      string    `json:"commit_id"`
		SubmittedAt   time.Time `json:"submitted_at"`
		CommentsCount int       `json:"comments_count"`
		Dismissed     bool      `json:"dismissed"`
		User          struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	if err := c.jsonCall(ctx, "gitea", http.MethodGet, base+"/pulls/"+id+"/reviews?limit=50", auth, nil, &reviews); err != nil {
		return nil, err
	}
	var convo []gtComment
	if err := c.jsonCall(ctx, "gitea", http.MethodGet, base+"/issues/"+id+"/comments", auth, nil, &convo); err != nil {
		return nil, err
	}
	ch := pr.change()
	ch.Reviews, ch.Threads = []Review{}, []Thread{}
	byLine := map[string]*Thread{}
	var order []string
	for _, rv := range reviews {
		verdict := map[string]string{"APPROVED": "approve", "REQUEST_CHANGES": "request_changes", "COMMENT": "comment"}[rv.State]
		if rv.Dismissed {
			verdict = "dismissed"
		}
		if verdict != "" && (verdict != "comment" || rv.Body != "") {
			ch.Reviews = append(ch.Reviews, Review{ExternalID: strconv.FormatInt(rv.ID, 10), Reviewer: rv.User.Login,
				Verdict: verdict, Body: rv.Body, CommitSHA: rv.CommitID, SubmittedAt: rv.SubmittedAt})
		}
		if rv.CommentsCount == 0 {
			continue
		}
		var comments []gtComment
		if err := c.jsonCall(ctx, "gitea", http.MethodGet, fmt.Sprintf("%s/pulls/%s/reviews/%d/comments", base, id, rv.ID),
			auth, nil, &comments); err != nil {
			return nil, err
		}
		// Gitea has no thread object: comments on the same line are one thread.
		for _, cm := range comments {
			side, line := "new", cm.Position
			if line == 0 {
				side, line = "old", cm.OriginalPosition
			}
			key := fmt.Sprintf("%s:%s:%d", cm.Path, side, line)
			th, ok := byLine[key]
			if !ok {
				th = &Thread{ExternalID: key, Path: cm.Path, Side: side, Line: line, OriginalLine: line,
					CommitSHA: cm.CommitID, OriginalCommit: cm.CommitID}
				byLine[key] = th
				order = append(order, key)
			}
			th.Comments = append(th.Comments, cm.comment())
		}
	}
	for _, key := range order {
		th := byLine[key]
		sort.SliceStable(th.Comments, func(i, j int) bool { return th.Comments[i].CreatedAt.Before(th.Comments[j].CreatedAt) })
		ch.Threads = append(ch.Threads, *th)
	}
	conv := Thread{ExternalID: ConversationThread, Comments: []Comment{}}
	for _, cm := range convo {
		conv.Comments = append(conv.Comments, cm.comment())
	}
	ch.Threads = append(ch.Threads, conv)
	return ch, nil
}

func (giteaProvider) ListOpenChanges(ctx context.Context, c *ChangeClient, repo RepoRef, v Validators) ([]ChangeRef, Validators, bool, error) {
	base, err := gtBase(repo)
	if err != nil {
		return nil, v, false, err
	}
	next := base + "/pulls?state=open&limit=50&sort=recentupdate"
	var refs []ChangeRef
	for page := 0; next != ""; page++ {
		if page == maxListPages {
			return refs, v, false, nil
		}
		req, err := http.NewRequest(http.MethodGet, next, nil)
		if err != nil {
			return nil, v, false, err
		}
		for k, val := range giteaAuth(repo.Token) {
			req.Header.Set(k, val)
		}
		resp, err := c.do(ctx, "gitea", req)
		if err != nil {
			return nil, v, false, err
		}
		var pulls []ghPull
		err = json.NewDecoder(resp.Body).Decode(&pulls)
		resp.Body.Close()
		if err != nil {
			return nil, v, false, fmt.Errorf("vcs: decode gitea pulls: %w", err)
		}
		for _, p := range pulls {
			refs = append(refs, ChangeRef{ID: strconv.Itoa(p.Number), HeadSHA: p.Head.SHA, UpdatedAt: p.UpdatedAt})
		}
		next = nextLink(resp.Header)
	}
	return refs, v, true, nil
}

// PublishCheck sets a commit status; branch protection's required status
// checks then hold the merge button.
func (giteaProvider) PublishCheck(ctx context.Context, c *ChangeClient, repo RepoRef, sha string, res CheckResult) error {
	base, err := gtBase(repo)
	if err != nil {
		return err
	}
	return c.jsonCall(ctx, "gitea", http.MethodPost, base+"/statuses/"+sha, giteaAuth(repo.Token),
		map[string]string{"state": res.State, "context": GateContext, "description": res.Description, "target_url": res.TargetURL}, nil)
}

// Merge merges only if the head is still ExpectedHead (head_commit_id).
func (giteaProvider) Merge(ctx context.Context, c *ChangeClient, repo RepoRef, id string, opts MergeOpts) (string, error) {
	base, err := gtBase(repo)
	if err != nil {
		return "", err
	}
	method := opts.Method
	if method == "" {
		method = "merge"
	}
	err = c.jsonCall(ctx, "gitea", http.MethodPost, base+"/pulls/"+id+"/merge", giteaAuth(repo.Token),
		map[string]string{"Do": method, "head_commit_id": opts.ExpectedHead}, nil)
	var se *StatusError
	switch {
	case errors.As(err, &se) && se.StatusCode == http.StatusConflict:
		return "", fmt.Errorf("%w (%s)", ErrHeadMoved, se.Message)
	case errors.As(err, &se) && (se.StatusCode == http.StatusMethodNotAllowed || se.StatusCode == http.StatusUnprocessableEntity):
		return "", fmt.Errorf("%w: %s", ErrNotMergeable, se.Message)
	case err != nil:
		return "", err
	}
	return "", nil // Gitea does not return the merge commit
}

// PostComment: a line comment (new thread or reply — Gitea threads are just
// comments on the same line) is a one-comment review; anything else is a
// conversation comment.
func (giteaProvider) PostComment(ctx context.Context, c *ChangeClient, repo RepoRef, id string, req CommentRequest) (string, error) {
	base, err := gtBase(repo)
	if err != nil {
		return "", err
	}
	auth := giteaAuth(repo.Token)
	var out struct {
		ID int64 `json:"id"`
	}
	if req.Anchor == nil {
		if err := c.jsonCall(ctx, "gitea", http.MethodPost, base+"/issues/"+id+"/comments", auth,
			map[string]string{"body": req.Body}, &out); err != nil {
			return "", err
		}
		return strconv.FormatInt(out.ID, 10), nil
	}
	cm := map[string]any{"path": req.Anchor.Path, "body": req.Body}
	if req.Anchor.Side == "old" {
		cm["old_position"] = req.Anchor.Line
	} else {
		cm["new_position"] = req.Anchor.Line
	}
	if err := c.jsonCall(ctx, "gitea", http.MethodPost, base+"/pulls/"+id+"/reviews", auth,
		map[string]any{"event": "COMMENT", "body": "", "commit_id": req.Anchor.CommitSHA, "comments": []any{cm}}, &out); err != nil {
		return "", err
	}
	return "review:" + strconv.FormatInt(out.ID, 10), nil
}

// ErrNoResolve: the host keeps no resolved state (Forge does).
var ErrNoResolve = errors.New("this host has no API to resolve a conversation")

func (giteaProvider) ResolveThread(context.Context, *ChangeClient, RepoRef, string, string, bool) error {
	return ErrNoResolve
}

func (giteaProvider) PostReview(ctx context.Context, c *ChangeClient, repo RepoRef, id, verdict, body, commitSHA string) (string, error) {
	base, err := gtBase(repo)
	if err != nil {
		return "", err
	}
	event := map[string]string{"approve": "APPROVED", "request_changes": "REQUEST_CHANGES", "comment": "COMMENT"}[verdict]
	if event == "" {
		return "", fmt.Errorf("unknown verdict %q", verdict)
	}
	var out struct {
		ID int64 `json:"id"`
	}
	if err := c.jsonCall(ctx, "gitea", http.MethodPost, base+"/pulls/"+id+"/reviews", giteaAuth(repo.Token),
		map[string]string{"event": event, "body": body, "commit_id": commitSHA}, &out); err != nil {
		return "", err
	}
	return strconv.FormatInt(out.ID, 10), nil
}
