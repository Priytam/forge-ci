package vcs

import (
	"bytes"
	"context"
	"encoding/json"
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
		MergeMethods: []string{"merge", "squash", "rebase"},
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
	default:
		return nil, nil // pushes, checks, comments (until threads ship): not a change update
	}
	var payload struct {
		Action      string  `json:"action"`
		PullRequest *ghPull `json:"pull_request"`
		Repository  struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
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
	return ch, nil
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
