package vcs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// Bitbucket Cloud change source and writers (API 2.0). A refresh reads the
// pull request and its comments; Bitbucket abbreviates commit hashes in pull
// requests, so the full head and target commits are looked up only when they
// moved. Its merge call cannot name an expected head, so Merge checks the
// head itself right before merging. Bitbucket answers no conditional
// requests: the reconcile costs one call per repo.

func (bitbucketProvider) Capabilities() Capabilities {
	return Capabilities{Changes: true, Threads: true, Resolve: true, Drafts: true,
		MergeMethods: []string{"merge", "squash", "fast_forward"}, StatusChecks: true, Reviews: true}
}

// MirrorRefs: Bitbucket has no pull-request refs; the two branches carry the
// commits (a pull request from a fork cannot be mirrored this way).
func (bitbucketProvider) MirrorRefs(_, source, target string) []string {
	return []string{headRef(source), headRef(target)}
}

func (c *ChangeClient) bbBase(repo RepoRef) (string, error) {
	owner, name, ok := splitRepo(repo.Repo)
	if !ok {
		return "", fmt.Errorf("invalid repo %q (want workspace/repo)", repo.Repo)
	}
	return c.bitbucketBase + "/2.0/repositories/" + owner + "/" + name, nil
}

type bbPull struct {
	ID          int    `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	State       string `json:"state"` // OPEN | MERGED | DECLINED | SUPERSEDED
	Draft       bool   `json:"draft"`
	Source      struct {
		Branch struct {
			Name string `json:"name"`
		} `json:"branch"`
		Commit struct {
			Hash string `json:"hash"`
		} `json:"commit"`
	} `json:"source"`
	Destination struct {
		Branch struct {
			Name string `json:"name"`
		} `json:"branch"`
		Commit struct {
			Hash string `json:"hash"`
		} `json:"commit"`
	} `json:"destination"`
	Author struct {
		Nickname string `json:"nickname"`
	} `json:"author"`
	Participants []struct {
		User struct {
			Nickname string `json:"nickname"`
		} `json:"user"`
		Approved      bool      `json:"approved"`
		State         string    `json:"state"` // approved | changes_requested | null
		ParticipantOn time.Time `json:"participated_on"`
	} `json:"participants"`
	Links struct {
		HTML struct {
			Href string `json:"href"`
		} `json:"html"`
	} `json:"links"`
	CreatedOn time.Time `json:"created_on"`
	UpdatedOn time.Time `json:"updated_on"`
}

type bbComment struct {
	ID      int64 `json:"id"`
	Content struct {
		Raw string `json:"raw"`
	} `json:"content"`
	User struct {
		Nickname string `json:"nickname"`
	} `json:"user"`
	Inline *struct {
		Path string `json:"path"`
		To   int    `json:"to"`   // line on the new side
		From int    `json:"from"` // line on the old side
	} `json:"inline"`
	Parent *struct {
		ID int64 `json:"id"`
	} `json:"parent"`
	Deleted    bool      `json:"deleted"`
	Resolution *struct{} `json:"resolution"`
	CreatedOn  time.Time `json:"created_on"`
	UpdatedOn  time.Time `json:"updated_on"`
}

func (bitbucketProvider) NormalizeEvent(hdr http.Header, body []byte) ([]ChangeEvent, error) {
	var kind string
	switch key := hdr.Get("X-Event-Key"); key {
	case "pullrequest:created", "pullrequest:updated", "pullrequest:fulfilled", "pullrequest:rejected":
		kind = "change"
	case "pullrequest:approved", "pullrequest:unapproved", "pullrequest:changes_request_created", "pullrequest:changes_request_removed":
		kind = "review"
	case "pullrequest:comment_created", "pullrequest:comment_updated", "pullrequest:comment_deleted",
		"pullrequest:comment_resolved", "pullrequest:comment_reopened":
		kind = "comment"
	default:
		return nil, nil
	}
	var payload struct {
		PullRequest struct {
			ID int `json:"id"`
		} `json:"pullrequest"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	if payload.PullRequest.ID == 0 || payload.Repository.FullName == "" {
		return nil, nil
	}
	return []ChangeEvent{{Repo: payload.Repository.FullName, ChangeID: strconv.Itoa(payload.PullRequest.ID), Kind: kind,
		Action: hdr.Get("X-Event-Key"), DeliveryID: hdr.Get("X-Request-UUID")}}, nil
}

// fullHash expands an abbreviated commit hash, reusing known when it matches.
func (c *ChangeClient) bbFullHash(ctx context.Context, base string, auth map[string]string, short, known string) (string, error) {
	if short == "" || (known != "" && SameCommit(known, short)) {
		return known, nil
	}
	var out struct {
		Hash string `json:"hash"`
	}
	if err := c.jsonCall(ctx, "bitbucket", http.MethodGet, base+"/commit/"+short, auth, nil, &out); err != nil {
		return "", err
	}
	return out.Hash, nil
}

func (bitbucketProvider) GetChange(ctx context.Context, c *ChangeClient, repo RepoRef, id string, prev *Change) (*Change, error) {
	base, err := c.bbBase(repo)
	if err != nil {
		return nil, err
	}
	auth := bearer(repo.Token)
	var pr bbPull
	if err := c.jsonCall(ctx, "bitbucket", http.MethodGet, base+"/pullrequests/"+id, auth, nil, &pr); err != nil {
		return nil, err
	}
	var knownHead, knownBase string
	if prev != nil {
		knownHead, knownBase = prev.HeadSHA, prev.BaseSHA
	}
	head, err := c.bbFullHash(ctx, base, auth, pr.Source.Commit.Hash, knownHead)
	if err != nil {
		return nil, err
	}
	target, err := c.bbFullHash(ctx, base, auth, pr.Destination.Commit.Hash, knownBase)
	if err != nil {
		return nil, err
	}
	var comments []bbComment
	next := base + "/pullrequests/" + id + "/comments?pagelen=100"
	for page := 0; next != "" && page < maxListPages; page++ {
		var out struct {
			Values []bbComment `json:"values"`
			Next   string      `json:"next"`
		}
		if err := c.jsonCall(ctx, "bitbucket", http.MethodGet, next, auth, nil, &out); err != nil {
			return nil, err
		}
		comments = append(comments, out.Values...)
		next = out.Next
	}
	state := map[string]string{"OPEN": "open", "MERGED": "merged"}[pr.State]
	if state == "" {
		state = "closed"
	}
	ch := &Change{ID: strconv.Itoa(pr.ID), Title: pr.Title, Body: pr.Description, Author: pr.Author.Nickname,
		SourceBranch: pr.Source.Branch.Name, TargetBranch: pr.Destination.Branch.Name, State: state, Draft: pr.Draft,
		HeadSHA: head, BaseSHA: target, Labels: []string{}, URL: pr.Links.HTML.Href, CreatedAt: pr.CreatedOn,
		UpdatedAt: pr.UpdatedOn, Reviews: []Review{}, Threads: []Thread{}}
	for _, p := range pr.Participants {
		verdict := ""
		switch {
		case p.State == "changes_requested":
			verdict = "request_changes"
		case p.Approved || p.State == "approved":
			verdict = "approve"
		}
		if verdict != "" {
			ch.Reviews = append(ch.Reviews, Review{ExternalID: "participant:" + p.User.Nickname, Reviewer: p.User.Nickname,
				Verdict: verdict, SubmittedAt: p.ParticipantOn})
		}
	}
	// Comments form trees by parent; each root is a thread.
	byID := map[int64]bbComment{}
	for _, cm := range comments {
		byID[cm.ID] = cm
	}
	rootOf := func(cm bbComment) bbComment {
		for i := 0; cm.Parent != nil && i < 100; i++ {
			p, ok := byID[cm.Parent.ID]
			if !ok {
				break
			}
			cm = p
		}
		return cm
	}
	threads := map[int64]*Thread{}
	var order []int64
	conv := Thread{ExternalID: ConversationThread, Comments: []Comment{}}
	for _, cm := range comments {
		if cm.Deleted {
			continue
		}
		c := Comment{ExternalID: strconv.FormatInt(cm.ID, 10), Author: cm.User.Nickname, Body: cm.Content.Raw,
			CreatedAt: cm.CreatedOn, UpdatedAt: cm.UpdatedOn}
		root := rootOf(cm)
		if root.Inline == nil {
			conv.Comments = append(conv.Comments, c)
			continue
		}
		th, ok := threads[root.ID]
		if !ok {
			th = &Thread{ExternalID: strconv.FormatInt(root.ID, 10), Path: root.Inline.Path, Side: "new",
				Line: root.Inline.To, OriginalLine: root.Inline.To, CommitSHA: head, OriginalCommit: head, Resolved: root.Resolution != nil}
			if root.Inline.To == 0 {
				th.Side, th.Line, th.OriginalLine = "old", root.Inline.From, root.Inline.From
			}
			threads[root.ID] = th
			order = append(order, root.ID)
		}
		th.Comments = append(th.Comments, c)
	}
	for _, id := range order {
		ch.Threads = append(ch.Threads, *threads[id])
	}
	ch.Threads = append(ch.Threads, conv)
	return ch, nil
}

func (bitbucketProvider) ListOpenChanges(ctx context.Context, c *ChangeClient, repo RepoRef, v Validators) ([]ChangeRef, Validators, bool, error) {
	base, err := c.bbBase(repo)
	if err != nil {
		return nil, v, false, err
	}
	next := base + "/pullrequests?state=OPEN&pagelen=50"
	var refs []ChangeRef
	for page := 0; next != ""; page++ {
		if page == maxListPages {
			return refs, v, false, nil
		}
		var out struct {
			Values []bbPull `json:"values"`
			Next   string   `json:"next"`
		}
		if err := c.jsonCall(ctx, "bitbucket", http.MethodGet, next, bearer(repo.Token), nil, &out); err != nil {
			return nil, v, false, err
		}
		for _, p := range out.Values {
			refs = append(refs, ChangeRef{ID: strconv.Itoa(p.ID), HeadSHA: p.Source.Commit.Hash, UpdatedAt: p.UpdatedOn})
		}
		next = out.Next
	}
	return refs, v, true, nil
}

// PublishCheck sets a build status; "minimum successful builds" in the
// branch's merge checks then holds the merge button.
func (bitbucketProvider) PublishCheck(ctx context.Context, c *ChangeClient, repo RepoRef, sha string, res CheckResult) error {
	base, err := c.bbBase(repo)
	if err != nil {
		return err
	}
	state := map[string]string{"success": "SUCCESSFUL", "failure": "FAILED"}[res.State]
	if state == "" {
		state = "INPROGRESS"
	}
	return c.jsonCall(ctx, "bitbucket", http.MethodPost, base+"/commit/"+sha+"/statuses/build", bearer(repo.Token),
		map[string]string{"key": GateContext, "state": state, "name": GateContext, "url": res.TargetURL, "description": res.Description}, nil)
}

// Merge: Bitbucket's merge takes no expected head, so the head is checked
// first; the window between the two calls is a fraction of a second.
func (bitbucketProvider) Merge(ctx context.Context, c *ChangeClient, repo RepoRef, id string, opts MergeOpts) (string, error) {
	base, err := c.bbBase(repo)
	if err != nil {
		return "", err
	}
	auth := bearer(repo.Token)
	var pr bbPull
	if err := c.jsonCall(ctx, "bitbucket", http.MethodGet, base+"/pullrequests/"+id, auth, nil, &pr); err != nil {
		return "", err
	}
	if !SameCommit(opts.ExpectedHead, pr.Source.Commit.Hash) {
		return "", ErrHeadMoved
	}
	strategy := map[string]string{"merge": "merge_commit", "squash": "squash", "fast_forward": "fast_forward"}[opts.Method]
	if strategy == "" {
		strategy = "merge_commit"
	}
	var out struct {
		MergeCommit struct {
			Hash string `json:"hash"`
		} `json:"merge_commit"`
	}
	err = c.jsonCall(ctx, "bitbucket", http.MethodPost, base+"/pullrequests/"+id+"/merge", auth,
		map[string]string{"type": "pullrequest", "merge_strategy": strategy}, &out)
	var se *StatusError
	switch {
	case errors.As(err, &se) && (se.StatusCode == http.StatusBadRequest || se.StatusCode == 555):
		return "", fmt.Errorf("%w: %s", ErrNotMergeable, se.Message)
	case err != nil:
		return "", err
	}
	return out.MergeCommit.Hash, nil
}

func (bitbucketProvider) PostComment(ctx context.Context, c *ChangeClient, repo RepoRef, id string, req CommentRequest) (string, error) {
	base, err := c.bbBase(repo)
	if err != nil {
		return "", err
	}
	in := map[string]any{"content": map[string]string{"raw": req.Body}}
	switch {
	case req.ReplyTo != "":
		parent, _ := strconv.ParseInt(req.ReplyTo, 10, 64)
		in["parent"] = map[string]int64{"id": parent}
	case req.Anchor != nil:
		inline := map[string]any{"path": req.Anchor.Path}
		if req.Anchor.Side == "old" {
			inline["from"] = req.Anchor.Line
		} else {
			inline["to"] = req.Anchor.Line
		}
		in["inline"] = inline
	}
	var out struct {
		ID int64 `json:"id"`
	}
	if err := c.jsonCall(ctx, "bitbucket", http.MethodPost, base+"/pullrequests/"+id+"/comments", bearer(repo.Token), in, &out); err != nil {
		return "", err
	}
	return strconv.FormatInt(out.ID, 10), nil
}

// ResolveThread resolves (POST) or reopens (DELETE) a comment thread.
func (bitbucketProvider) ResolveThread(ctx context.Context, c *ChangeClient, repo RepoRef, id, threadID string, resolved bool) error {
	base, err := c.bbBase(repo)
	if err != nil {
		return err
	}
	method := http.MethodDelete
	if resolved {
		method = http.MethodPost
	}
	return c.jsonCall(ctx, "bitbucket", method, base+"/pullrequests/"+id+"/comments/"+threadID+"/resolve", bearer(repo.Token), nil, nil)
}

func (bitbucketProvider) PostReview(ctx context.Context, c *ChangeClient, repo RepoRef, id, verdict, body, _ string) (string, error) {
	base, err := c.bbBase(repo)
	if err != nil {
		return "", err
	}
	switch verdict {
	case "approve":
		return "approve", c.jsonCall(ctx, "bitbucket", http.MethodPost, base+"/pullrequests/"+id+"/approve", bearer(repo.Token), nil, nil)
	case "request_changes":
		return "request-changes", c.jsonCall(ctx, "bitbucket", http.MethodPost, base+"/pullrequests/"+id+"/request-changes", bearer(repo.Token), nil, nil)
	case "comment":
		return (bitbucketProvider{}).PostComment(ctx, c, repo, id, CommentRequest{Body: body})
	}
	return "", fmt.Errorf("unknown verdict %q", verdict)
}
