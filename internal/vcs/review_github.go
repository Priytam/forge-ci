package vcs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// GitHub's change source. A refresh is ONE GraphQL query (change + reviews,
// about one point of the 5,000/hour budget); the reconcile list is REST with
// If-None-Match, because a 304 there costs nothing.

func (githubProvider) Capabilities() Capabilities {
	return Capabilities{
		Changes: true, Threads: true, Suggestions: true, Resolve: true, Drafts: true,
		MergeMethods: []string{"merge", "squash", "rebase"}, StatusChecks: true,
	}
}

// ghPull is the REST / webhook shape of a pull request.
type ghPull struct {
	Number  int    `json:"number"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	State   string `json:"state"` // open | closed
	Draft   bool   `json:"draft"`
	Merged  bool   `json:"merged"`
	HTMLURL string `json:"html_url"`
	User    struct {
		Login string `json:"login"`
	} `json:"user"`
	Head struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"base"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
	MergedAt  *time.Time `json:"merged_at"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

func (p ghPull) change() *Change {
	state := p.State
	if p.Merged || p.MergedAt != nil {
		state = "merged"
	}
	labels := []string{}
	for _, l := range p.Labels {
		labels = append(labels, l.Name)
	}
	return &Change{
		ID: strconv.Itoa(p.Number), Title: p.Title, Body: p.Body, Author: p.User.Login,
		SourceBranch: p.Head.Ref, TargetBranch: p.Base.Ref, State: state, Draft: p.Draft,
		HeadSHA: p.Head.SHA, BaseSHA: p.Base.SHA, Labels: labels, URL: p.HTMLURL,
		CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
	}
}

func (githubProvider) NormalizeEvent(hdr http.Header, body []byte) ([]ChangeEvent, error) {
	event := hdr.Get("X-GitHub-Event")
	var kind string
	switch event {
	case "pull_request":
		kind = "change"
	case "pull_request_review":
		kind = "review"
	case "pull_request_review_comment", "pull_request_review_thread", "issue_comment":
		kind = "comment"
	default:
		return nil, nil // pushes, checks: not a change update
	}
	var payload struct {
		Action      string  `json:"action"`
		PullRequest *ghPull `json:"pull_request"`
		Issue       *struct {
			Number      int       `json:"number"`
			PullRequest *struct{} `json:"pull_request"` // set only when the issue is a pull request
		} `json:"issue"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	if payload.PullRequest == nil && payload.Issue != nil && payload.Issue.PullRequest != nil {
		payload.PullRequest = &ghPull{Number: payload.Issue.Number} // a comment on a pull request's conversation
	}
	if payload.PullRequest == nil || payload.Repository.FullName == "" {
		return nil, nil
	}
	ev := ChangeEvent{
		Repo: payload.Repository.FullName, ChangeID: strconv.Itoa(payload.PullRequest.Number),
		Kind: kind, Action: payload.Action, DeliveryID: hdr.Get("X-GitHub-Delivery"),
	}
	// A pull_request payload is the whole change (reviews aside). A review
	// payload's pull_request lacks merge state, so it only triggers a refresh.
	if kind == "change" {
		ev.Snapshot = payload.PullRequest.change()
	}
	return []ChangeEvent{ev}, nil
}

// githubGraphQL derives the GraphQL endpoint from the REST base:
// https://api.github.com → /graphql; GHE https://host/api/v3 → /api/graphql.
func githubGraphQL(restBase string) string {
	if strings.HasSuffix(restBase, "/api/v3") {
		return strings.TrimSuffix(restBase, "/v3") + "/graphql"
	}
	return restBase + "/graphql"
}

const githubChangeQuery = `query($owner:String!,$name:String!,$number:Int!){
  repository(owner:$owner,name:$name){
    pullRequest(number:$number){
      number title body url state isDraft createdAt updatedAt
      headRefName headRefOid baseRefName baseRefOid
      author{login}
      labels(first:50){nodes{name}}
      reviews(last:100){nodes{databaseId state body submittedAt author{login} commit{oid}}}
      reviewThreads(first:100){nodes{id isResolved isOutdated path line startLine originalLine diffSide
        comments(first:100){nodes{databaseId body createdAt updatedAt author{login} commit{oid} originalCommit{oid}}}}}
      comments(last:100){nodes{databaseId body createdAt updatedAt author{login}}}
    }
  }
}`

func (githubProvider) GetChange(ctx context.Context, c *ChangeClient, repo RepoRef, id string, _ *Change) (*Change, error) {
	owner, name, ok := splitRepo(repo.Repo)
	if !ok {
		return nil, fmt.Errorf("invalid repo %q (want owner/name)", repo.Repo)
	}
	number, err := strconv.Atoi(id)
	if err != nil {
		return nil, fmt.Errorf("invalid pull request number %q", id)
	}
	body, _ := json.Marshal(map[string]any{
		"query":     githubChangeQuery,
		"variables": map[string]any{"owner": owner, "name": name, "number": number},
	})
	req, err := http.NewRequest(http.MethodPost, githubGraphQL(c.githubBase), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range bearer(repo.Token) {
		req.Header.Set(k, v)
	}
	resp, err := c.do(ctx, "github", req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Data struct {
			Repository *struct {
				PullRequest *struct {
					Number      int       `json:"number"`
					Title       string    `json:"title"`
					Body        string    `json:"body"`
					URL         string    `json:"url"`
					State       string    `json:"state"` // OPEN | CLOSED | MERGED
					IsDraft     bool      `json:"isDraft"`
					CreatedAt   time.Time `json:"createdAt"`
					UpdatedAt   time.Time `json:"updatedAt"`
					HeadRefName string    `json:"headRefName"`
					HeadRefOid  string    `json:"headRefOid"`
					BaseRefName string    `json:"baseRefName"`
					BaseRefOid  string    `json:"baseRefOid"`
					Author      *struct {
						Login string `json:"login"`
					} `json:"author"`
					Labels struct {
						Nodes []struct {
							Name string `json:"name"`
						} `json:"nodes"`
					} `json:"labels"`
					Reviews struct {
						Nodes []struct {
							DatabaseID  int64      `json:"databaseId"`
							State       string     `json:"state"`
							Body        string     `json:"body"`
							SubmittedAt *time.Time `json:"submittedAt"`
							Author      *struct {
								Login string `json:"login"`
							} `json:"author"`
							Commit *struct {
								OID string `json:"oid"`
							} `json:"commit"`
						} `json:"nodes"`
					} `json:"reviews"`
					ReviewThreads struct {
						Nodes []struct {
							ID           string `json:"id"`
							IsResolved   bool   `json:"isResolved"`
							IsOutdated   bool   `json:"isOutdated"`
							Path         string `json:"path"`
							Line         *int   `json:"line"`
							StartLine    *int   `json:"startLine"`
							OriginalLine *int   `json:"originalLine"`
							DiffSide     string `json:"diffSide"` // LEFT | RIGHT
							Comments     struct {
								Nodes []ghComment `json:"nodes"`
							} `json:"comments"`
						} `json:"nodes"`
					} `json:"reviewThreads"`
					Comments struct {
						Nodes []ghComment `json:"nodes"`
					} `json:"comments"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
		Errors []struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("vcs: decode github change: %w", err)
	}
	if out.Data.Repository == nil || out.Data.Repository.PullRequest == nil {
		for _, e := range out.Errors {
			if e.Type != "NOT_FOUND" {
				return nil, fmt.Errorf("vcs: github graphql: %s", e.Type)
			}
		}
		return nil, ErrChangeNotFound
	}
	pr := out.Data.Repository.PullRequest
	ch := &Change{
		ID: strconv.Itoa(pr.Number), Title: pr.Title, Body: pr.Body, URL: pr.URL,
		State: strings.ToLower(pr.State), Draft: pr.IsDraft,
		SourceBranch: pr.HeadRefName, TargetBranch: pr.BaseRefName,
		HeadSHA: pr.HeadRefOid, BaseSHA: pr.BaseRefOid,
		CreatedAt: pr.CreatedAt, UpdatedAt: pr.UpdatedAt,
		Labels: []string{}, Reviews: []Review{},
	}
	if pr.Author != nil {
		ch.Author = pr.Author.Login
	}
	for _, l := range pr.Labels.Nodes {
		ch.Labels = append(ch.Labels, l.Name)
	}
	for _, r := range pr.Reviews.Nodes {
		verdict := githubVerdict(r.State)
		if verdict == "" || r.SubmittedAt == nil {
			continue // pending (unsubmitted) reviews are private to their author
		}
		rv := Review{
			ExternalID: strconv.FormatInt(r.DatabaseID, 10), Verdict: verdict,
			Body: r.Body, SubmittedAt: *r.SubmittedAt,
		}
		if r.Author != nil {
			rv.Reviewer = r.Author.Login
		}
		if r.Commit != nil {
			rv.CommitSHA = r.Commit.OID
		}
		ch.Reviews = append(ch.Reviews, rv)
	}
	ch.Threads = []Thread{}
	for _, t := range pr.ReviewThreads.Nodes {
		th := Thread{ExternalID: t.ID, Path: t.Path, Side: "new", Resolved: t.IsResolved, Outdated: t.IsOutdated,
			Line: deref(t.Line), StartLine: deref(t.StartLine), OriginalLine: deref(t.OriginalLine)}
		if t.DiffSide == "LEFT" {
			th.Side = "old"
		}
		for i, cm := range t.Comments.Nodes {
			if i == 0 {
				if cm.Commit != nil {
					th.CommitSHA = cm.Commit.OID
				}
				if cm.OriginalCommit != nil {
					th.OriginalCommit = cm.OriginalCommit.OID
				}
			}
			th.Comments = append(th.Comments, cm.comment())
		}
		ch.Threads = append(ch.Threads, th)
	}
	conv := Thread{ExternalID: ConversationThread, Comments: []Comment{}}
	for _, cm := range pr.Comments.Nodes {
		conv.Comments = append(conv.Comments, cm.comment())
	}
	ch.Threads = append(ch.Threads, conv)
	return ch, nil
}

// ghComment is a GraphQL comment (review comment or conversation comment).
type ghComment struct {
	DatabaseID int64     `json:"databaseId"`
	Body       string    `json:"body"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
	Author     *struct {
		Login string `json:"login"`
	} `json:"author"`
	Commit *struct {
		OID string `json:"oid"`
	} `json:"commit"`
	OriginalCommit *struct {
		OID string `json:"oid"`
	} `json:"originalCommit"`
}

func (c ghComment) comment() Comment {
	out := Comment{ExternalID: strconv.FormatInt(c.DatabaseID, 10), Body: c.Body, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt}
	if c.Author != nil {
		out.Author = c.Author.Login
	}
	return out
}

func deref(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// githubVerdict maps a GraphQL review state onto Forge's verdicts ("" = skip).
func githubVerdict(state string) string {
	switch state {
	case "APPROVED":
		return "approve"
	case "CHANGES_REQUESTED":
		return "request_changes"
	case "COMMENTED":
		return "comment"
	case "DISMISSED":
		return "dismissed"
	}
	return ""
}

// maxListPages bounds a reconcile list (100 per page).
const maxListPages = 10

func (githubProvider) ListOpenChanges(ctx context.Context, c *ChangeClient, repo RepoRef, v Validators) ([]ChangeRef, Validators, bool, error) {
	owner, name, ok := splitRepo(repo.Repo)
	if !ok {
		return nil, v, false, fmt.Errorf("invalid repo %q (want owner/name)", repo.Repo)
	}
	next := fmt.Sprintf("%s/repos/%s/%s/pulls?state=open&per_page=100&sort=updated&direction=desc",
		c.githubBase, owner, name)
	var refs []ChangeRef
	var got Validators
	for page := 0; next != ""; page++ {
		if page == maxListPages {
			return refs, got, false, nil
		}
		req, err := http.NewRequest(http.MethodGet, next, nil)
		if err != nil {
			return nil, v, false, err
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		for k, val := range bearer(repo.Token) {
			req.Header.Set(k, val)
		}
		if page == 0 {
			// Only the first page is conditional: it changes whenever any
			// open pull request does, because the list is sorted by update.
			if v.ETag != "" {
				req.Header.Set("If-None-Match", v.ETag)
			}
			if v.LastModified != "" {
				req.Header.Set("If-Modified-Since", v.LastModified)
			}
		}
		resp, err := c.do(ctx, "github", req)
		if err != nil {
			return nil, v, false, err
		}
		if resp.StatusCode == http.StatusNotModified {
			resp.Body.Close()
			return nil, v, true, ErrNotModified
		}
		if page == 0 {
			got = Validators{ETag: resp.Header.Get("ETag"), LastModified: resp.Header.Get("Last-Modified")}
		}
		var pulls []ghPull
		err = json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&pulls)
		resp.Body.Close()
		if err != nil {
			return nil, v, false, fmt.Errorf("vcs: decode github pulls: %w", err)
		}
		for _, p := range pulls {
			refs = append(refs, ChangeRef{ID: strconv.Itoa(p.Number), HeadSHA: p.Head.SHA, UpdatedAt: p.UpdatedAt})
		}
		next = nextLink(resp.Header)
	}
	return refs, got, true, nil
}

// PublishCheck sets the forge/merge-gate commit status on sha. Statuses are
// keyed by context, so re-sending the same result is harmless.
func (githubProvider) PublishCheck(ctx context.Context, c *ChangeClient, repo RepoRef, sha string, res CheckResult) error {
	owner, name, ok := splitRepo(repo.Repo)
	if !ok {
		return fmt.Errorf("invalid repo %q (want owner/name)", repo.Repo)
	}
	desc := res.Description
	if len(desc) > 140 {
		desc = desc[:137] + "..."
	}
	body, _ := json.Marshal(map[string]string{
		"state": res.State, "context": GateContext, "description": desc, "target_url": res.TargetURL,
	})
	req, err := http.NewRequest(http.MethodPost,
		fmt.Sprintf("%s/repos/%s/%s/statuses/%s", c.githubBase, owner, name, sha), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/vnd.github+json")
	for k, v := range bearer(repo.Token) {
		req.Header.Set(k, v)
	}
	resp, err := c.do(ctx, "github", req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// Merge merges the pull request only if its head is still ExpectedHead
// (GitHub answers 409 otherwise). 405 is GitHub's "not mergeable": conflicts,
// branch protection, or a required check — its message says which.
func (githubProvider) Merge(ctx context.Context, c *ChangeClient, repo RepoRef, id string, opts MergeOpts) (string, error) {
	owner, name, ok := splitRepo(repo.Repo)
	if !ok {
		return "", fmt.Errorf("invalid repo %q (want owner/name)", repo.Repo)
	}
	method := opts.Method
	if method == "" {
		method = "merge"
	}
	body, _ := json.Marshal(map[string]string{"sha": opts.ExpectedHead, "merge_method": method})
	req, err := http.NewRequest(http.MethodPut,
		fmt.Sprintf("%s/repos/%s/%s/pulls/%s/merge", c.githubBase, owner, name, id), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/vnd.github+json")
	for k, v := range bearer(repo.Token) {
		req.Header.Set(k, v)
	}
	resp, err := c.do(ctx, "github", req)
	var se *StatusError
	switch {
	case errors.As(err, &se) && se.StatusCode == http.StatusConflict:
		return "", fmt.Errorf("%w (%s)", ErrHeadMoved, se.Message)
	case errors.As(err, &se) && se.StatusCode == http.StatusMethodNotAllowed:
		return "", fmt.Errorf("%w: %s", ErrNotMergeable, se.Message)
	case err != nil:
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		SHA    string `json:"sha"`
		Merged bool   `json:"merged"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", fmt.Errorf("vcs: decode github merge: %w", err)
	}
	if !out.Merged {
		return "", ErrNotMergeable
	}
	return out.SHA, nil
}

// MirrorRefs: the target branch and GitHub's pull/<n>/head, which keeps a
// pull request's commits even from forks and after the branch is deleted.
func (githubProvider) MirrorRefs(id, _, target string) []string {
	return []string{headRef(target), "+refs/pull/" + id + "/head:refs/pull/" + id + "/head"}
}

// githubJSON sends a JSON request to the GitHub API and decodes the answer
// into out (when not nil).
func githubJSON(ctx context.Context, c *ChangeClient, repo RepoRef, method, url string, in, out any) error {
	body, _ := json.Marshal(in)
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/vnd.github+json")
	for k, v := range bearer(repo.Token) {
		req.Header.Set(k, v)
	}
	resp, err := c.do(ctx, "github", req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out)
}

// PostComment: a new line thread (POST pulls/{n}/comments), a reply
// (POST pulls/{n}/comments/{id}/replies), or a conversation comment
// (POST issues/{n}/comments). Returns the new comment's id.
func (githubProvider) PostComment(ctx context.Context, c *ChangeClient, repo RepoRef, id string, req CommentRequest) (string, error) {
	owner, name, ok := splitRepo(repo.Repo)
	if !ok {
		return "", fmt.Errorf("invalid repo %q (want owner/name)", repo.Repo)
	}
	base := fmt.Sprintf("%s/repos/%s/%s", c.githubBase, owner, name)
	var url string
	payload := map[string]any{"body": req.Body}
	switch {
	case req.Anchor != nil:
		url = fmt.Sprintf("%s/pulls/%s/comments", base, id)
		side := "RIGHT"
		if req.Anchor.Side == "old" {
			side = "LEFT"
		}
		payload["commit_id"], payload["path"], payload["line"], payload["side"] =
			req.Anchor.CommitSHA, req.Anchor.Path, req.Anchor.Line, side
		if req.Anchor.StartLine > 0 && req.Anchor.StartLine < req.Anchor.Line {
			payload["start_line"], payload["start_side"] = req.Anchor.StartLine, side
		}
	case req.ReplyTo != "":
		url = fmt.Sprintf("%s/pulls/%s/comments/%s/replies", base, id, req.ReplyTo)
	default:
		url = fmt.Sprintf("%s/issues/%s/comments", base, id)
	}
	var out struct {
		ID int64 `json:"id"`
	}
	if err := githubJSON(ctx, c, repo, http.MethodPost, url, payload, &out); err != nil {
		return "", err
	}
	return strconv.FormatInt(out.ID, 10), nil
}

// ResolveThread resolves or reopens a review thread (GraphQL only).
func (githubProvider) ResolveThread(ctx context.Context, c *ChangeClient, repo RepoRef, threadID string, resolved bool) error {
	mutation := "unresolveReviewThread"
	if resolved {
		mutation = "resolveReviewThread"
	}
	var out struct {
		Errors []struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	err := githubJSON(ctx, c, repo, http.MethodPost, githubGraphQL(c.githubBase), map[string]any{
		"query":     "mutation($id:ID!){" + mutation + "(input:{threadId:$id}){thread{id isResolved}}}",
		"variables": map[string]any{"id": threadID},
	}, &out)
	if err != nil {
		return err
	}
	if len(out.Errors) > 0 {
		if out.Errors[0].Type == "NOT_FOUND" {
			return ErrChangeNotFound
		}
		return &StatusError{Provider: "github", StatusCode: http.StatusUnprocessableEntity, Message: out.Errors[0].Message}
	}
	return nil
}

// PostReview submits a review on commitSHA.
func (githubProvider) PostReview(ctx context.Context, c *ChangeClient, repo RepoRef, id, verdict, body, commitSHA string) (string, error) {
	owner, name, ok := splitRepo(repo.Repo)
	if !ok {
		return "", fmt.Errorf("invalid repo %q (want owner/name)", repo.Repo)
	}
	event := map[string]string{"approve": "APPROVE", "request_changes": "REQUEST_CHANGES", "comment": "COMMENT"}[verdict]
	if event == "" {
		return "", fmt.Errorf("unknown verdict %q", verdict)
	}
	var out struct {
		ID int64 `json:"id"`
	}
	err := githubJSON(ctx, c, repo, http.MethodPost, fmt.Sprintf("%s/repos/%s/%s/pulls/%s/reviews", c.githubBase, owner, name, id),
		map[string]string{"event": event, "body": body, "commit_id": commitSHA}, &out)
	if err != nil {
		return "", err
	}
	return strconv.FormatInt(out.ID, 10), nil
}
