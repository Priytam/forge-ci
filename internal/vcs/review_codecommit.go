package vcs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/codecommit"
	cctypes "github.com/aws/aws-sdk-go-v2/service/codecommit/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
)

// AWS CodeCommit change source and writers. Calls are SigV4-signed SDK calls
// with the control plane's AWS identity (no token), counted like HTTP calls.
// Events arrive from EventBridge. CodeCommit has no commit statuses, so the
// merge gate is Forge's own approval on the pull request: an approval rule
// (template) requiring Forge's IAM role holds the merge until the gate
// passes. It has no resolved state and no list that says what changed: the
// reconcile only finds new and closed pull requests (events refresh the
// rest), and Forge keeps the resolved flag. Comments carry CodeCommit's own
// client request token, so a retried post never doubles.

func (codecommitProvider) Capabilities() Capabilities {
	return Capabilities{Changes: true, Threads: true, MergeMethods: []string{"merge", "squash", "fast_forward"}, StatusChecks: true}
}

func ccRepo(repo RepoRef) (CodeCommitRepo, error) {
	r, ok := ParseCodeCommitURL(repo.CloneURL)
	if repo.Region != "" {
		r.Region, ok = repo.Region, ok || r.Name != ""
	}
	if repo.Profile != "" {
		r.Profile = repo.Profile
	}
	if r.Name == "" {
		r.Name = repo.Repo
	}
	if r.Region == "" || r.Name == "" {
		return r, fmt.Errorf("codecommit repo %q has no region", repo.Repo)
	}
	return r, nil
}

// ccCall runs one SDK call through the read-path guard and the accounting.
// Throttling becomes a RateLimitedError; "not found" faults ErrChangeNotFound.
func (c *ChangeClient) ccCall(ctx context.Context, fn func() error) error {
	if err := c.guard(ctx, "codecommit"); err != nil {
		return err
	}
	err := fn()
	var api smithy.APIError
	switch {
	case err == nil:
		c.report(ctx, CallInfo{Provider: "codecommit", Result: "ok"})
		return nil
	case errors.As(err, &api) && (api.ErrorCode() == "ThrottlingException" || api.ErrorCode() == "TooManyRequestsException"):
		c.report(ctx, CallInfo{Provider: "codecommit", Result: "rate_limited"})
		return &RateLimitedError{RetryAfter: time.Minute, StatusCode: http.StatusTooManyRequests}
	case errors.As(err, &api) && (api.ErrorCode() == "PullRequestDoesNotExistException" || api.ErrorCode() == "RepositoryDoesNotExistException"):
		c.report(ctx, CallInfo{Provider: "codecommit", Result: "error"})
		return ErrChangeNotFound
	}
	c.report(ctx, CallInfo{Provider: "codecommit", Result: "error"})
	if errors.As(err, &api) {
		return &StatusError{Provider: "codecommit", StatusCode: http.StatusBadRequest, Message: api.ErrorCode()}
	}
	return err
}

func (c *ChangeClient) ccClient(ctx context.Context, repo RepoRef) (*codecommit.Client, CodeCommitRepo, error) {
	r, err := ccRepo(repo)
	if err != nil {
		return nil, r, err
	}
	cl, err := c.cc.client(ctx, r.Region, repo.RoleARN, r.Profile)
	return cl, r, err
}

// ccIdentity is Forge's own AWS identity (its approvals are the gate, not a
// review): CODECOMMIT_FORGE_ARN, else STS, asked once.
var ccIdentity struct {
	sync.Mutex
	arn string
}

func (c *ChangeClient) ccSelf(ctx context.Context, repo RepoRef) string {
	if v := os.Getenv("CODECOMMIT_FORGE_ARN"); v != "" {
		return v
	}
	ccIdentity.Lock()
	defer ccIdentity.Unlock()
	if ccIdentity.arn != "" {
		return ccIdentity.arn
	}
	r, err := ccRepo(repo)
	if err != nil {
		return ""
	}
	cl, err := c.cc.client(ctx, r.Region, repo.RoleARN, r.Profile)
	if err != nil {
		return ""
	}
	out, err := sts.New(sts.Options{Region: r.Region, Credentials: cl.Options().Credentials}).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err == nil && out.Arn != nil {
		ccIdentity.arn = *out.Arn
	}
	return ccIdentity.arn
}

// arnPrincipal reduces an IAM/STS ARN to its principal ("assumed-role/Role"
// or "user/name"), so a role matches whatever session it was used in.
func arnPrincipal(arn string) string {
	_, rest, _ := strings.Cut(arn, ":"+"assumed-role/")
	if rest != "" {
		role, _, _ := strings.Cut(rest, "/")
		return "assumed-role/" + role
	}
	if i := strings.LastIndex(arn, ":"); i >= 0 {
		return arn[i+1:]
	}
	return arn
}

// arnName is the human part of an ARN: the session or user name.
func arnName(arn string) string {
	return arn[strings.LastIndex(arn, "/")+1:]
}

func (codecommitProvider) NormalizeEvent(_ http.Header, body []byte) ([]ChangeEvent, error) {
	var env struct {
		ID         string `json:"id"`
		DetailType string `json:"detail-type"`
		Detail     struct {
			Event           string   `json:"event"`
			PullRequestID   string   `json:"pullRequestId"`
			RepositoryNames []string `json:"repositoryNames"`
			RepositoryName  string   `json:"repositoryName"`
		} `json:"detail"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, err
	}
	d := env.Detail
	if d.PullRequestID == "" {
		return nil, nil // a push, not a pull request
	}
	repo := d.RepositoryName
	if repo == "" && len(d.RepositoryNames) > 0 {
		repo = d.RepositoryNames[0]
	}
	kind := "change"
	switch {
	case strings.HasPrefix(d.Event, "commentOn") || strings.Contains(env.DetailType, "Comment"):
		kind = "comment"
	case strings.Contains(d.Event, "Approval"):
		kind = "review"
	}
	if repo == "" {
		return nil, nil
	}
	return []ChangeEvent{{Repo: repo, ChangeID: d.PullRequestID, Kind: kind, Action: d.Event, DeliveryID: env.ID}}, nil
}

func (codecommitProvider) GetChange(ctx context.Context, c *ChangeClient, repo RepoRef, id string, _ *Change) (*Change, error) {
	cl, r, err := c.ccClient(ctx, repo)
	if err != nil {
		return nil, err
	}
	var pr *cctypes.PullRequest
	if err := c.ccCall(ctx, func() error {
		out, err := cl.GetPullRequest(ctx, &codecommit.GetPullRequestInput{PullRequestId: aws.String(id)})
		if err == nil {
			pr = out.PullRequest
		}
		return err
	}); err != nil {
		return nil, err
	}
	if pr == nil || len(pr.PullRequestTargets) == 0 {
		return nil, ErrChangeNotFound
	}
	t := pr.PullRequestTargets[0]
	state := "open"
	if pr.PullRequestStatus == cctypes.PullRequestStatusEnumClosed {
		state = "closed"
		if t.MergeMetadata != nil && t.MergeMetadata.IsMerged {
			state = "merged"
		}
	}
	ch := &Change{ID: id, Title: aws.ToString(pr.Title), Body: aws.ToString(pr.Description), Author: arnName(aws.ToString(pr.AuthorArn)),
		SourceBranch: strings.TrimPrefix(aws.ToString(t.SourceReference), "refs/heads/"),
		TargetBranch: strings.TrimPrefix(aws.ToString(t.DestinationReference), "refs/heads/"), State: state,
		HeadSHA: aws.ToString(t.SourceCommit), BaseSHA: aws.ToString(t.DestinationCommit), Labels: []string{},
		URL: fmt.Sprintf("https://%s.console.aws.amazon.com/codesuite/codecommit/repositories/%s/pull-requests/%s/details?region=%s",
			r.Region, r.Name, id, r.Region),
		CreatedAt: aws.ToTime(pr.CreationDate), UpdatedAt: aws.ToTime(pr.LastActivityDate), Reviews: []Review{}, Threads: []Thread{}}
	self := arnPrincipal(c.ccSelf(ctx, repo))
	if err := c.ccCall(ctx, func() error {
		out, err := cl.GetPullRequestApprovalStates(ctx, &codecommit.GetPullRequestApprovalStatesInput{
			PullRequestId: aws.String(id), RevisionId: pr.RevisionId})
		if err != nil {
			return err
		}
		for _, a := range out.Approvals {
			arn := aws.ToString(a.UserArn)
			if a.ApprovalState != cctypes.ApprovalStateApprove || (self != "" && arnPrincipal(arn) == self) {
				continue // Forge's own approval is the gate, not a review
			}
			ch.Reviews = append(ch.Reviews, Review{ExternalID: "approval:" + arn, Reviewer: arnName(arn), Verdict: "approve",
				CommitSHA: ch.HeadSHA, SubmittedAt: ch.UpdatedAt})
		}
		return nil
	}); err != nil {
		return nil, err
	}
	conv := Thread{ExternalID: ConversationThread, Comments: []Comment{}}
	var token *string
	for page := 0; page < maxListPages; page++ {
		var out *codecommit.GetCommentsForPullRequestOutput
		if err := c.ccCall(ctx, func() error {
			var err error
			out, err = cl.GetCommentsForPullRequest(ctx, &codecommit.GetCommentsForPullRequestInput{
				PullRequestId: aws.String(id), RepositoryName: aws.String(r.Name), NextToken: token})
			return err
		}); err != nil {
			return nil, err
		}
		for _, data := range out.CommentsForPullRequestData {
			ccThreads(data, ch, &conv)
		}
		if out.NextToken == nil || *out.NextToken == "" {
			break
		}
		token = out.NextToken
	}
	ch.Threads = append(ch.Threads, conv)
	return ch, nil
}

// ccThreads turns one location's comments into threads: each comment that
// replies to nobody starts one; replies join their root's.
func ccThreads(data cctypes.CommentsForPullRequest, ch *Change, conv *Thread) {
	byID := map[string]cctypes.Comment{}
	for _, cm := range data.Comments {
		byID[aws.ToString(cm.CommentId)] = cm
	}
	rootOf := func(cm cctypes.Comment) string {
		id := aws.ToString(cm.CommentId)
		for i := 0; i < 100 && cm.InReplyTo != nil; i++ {
			p, ok := byID[aws.ToString(cm.InReplyTo)]
			if !ok {
				break
			}
			cm, id = p, aws.ToString(p.CommentId)
		}
		return id
	}
	threads := map[string]*Thread{}
	var order []string
	for _, cm := range data.Comments {
		if cm.Deleted {
			continue
		}
		c := Comment{ExternalID: aws.ToString(cm.CommentId), Author: arnName(aws.ToString(cm.AuthorArn)),
			Body: aws.ToString(cm.Content), CreatedAt: aws.ToTime(cm.CreationDate), UpdatedAt: aws.ToTime(cm.LastModifiedDate)}
		loc := data.Location
		if loc == nil || loc.FilePath == nil {
			conv.Comments = append(conv.Comments, c)
			continue
		}
		root := rootOf(cm)
		th, ok := threads[root]
		if !ok {
			line := int(aws.ToInt64(loc.FilePosition))
			th = &Thread{ExternalID: root, Path: aws.ToString(loc.FilePath), Side: "new", Line: line, OriginalLine: line,
				CommitSHA: aws.ToString(data.AfterCommitId), OriginalCommit: aws.ToString(data.AfterCommitId)}
			if loc.RelativeFileVersion == cctypes.RelativeFileVersionEnumBefore {
				th.Side = "old"
			}
			th.Outdated = th.CommitSHA != ch.HeadSHA
			if th.Outdated {
				th.Line = 0
			}
			threads[root] = th
			order = append(order, root)
		}
		th.Comments = append(th.Comments, c)
	}
	for _, id := range order {
		ch.Threads = append(ch.Threads, *threads[id])
	}
}

// ListOpenChanges lists ids only (CodeCommit's list says nothing more); the
// reconcile uses it to find new and closed pull requests.
func (codecommitProvider) ListOpenChanges(ctx context.Context, c *ChangeClient, repo RepoRef, v Validators) ([]ChangeRef, Validators, bool, error) {
	cl, r, err := c.ccClient(ctx, repo)
	if err != nil {
		return nil, v, false, err
	}
	var refs []ChangeRef
	var token *string
	for page := 0; page < maxListPages; page++ {
		var out *codecommit.ListPullRequestsOutput
		if err := c.ccCall(ctx, func() error {
			var err error
			out, err = cl.ListPullRequests(ctx, &codecommit.ListPullRequestsInput{RepositoryName: aws.String(r.Name),
				PullRequestStatus: cctypes.PullRequestStatusEnumOpen, NextToken: token})
			return err
		}); err != nil {
			return nil, v, false, err
		}
		for _, id := range out.PullRequestIds {
			refs = append(refs, ChangeRef{ID: id})
		}
		if out.NextToken == nil || *out.NextToken == "" {
			return refs, v, true, nil
		}
		token = out.NextToken
	}
	return refs, v, false, nil
}

// PublishCheck: Forge approves the pull request's current revision when the
// gate passes and revokes its approval otherwise. An approval rule requiring
// Forge's role (an approval rule template on the repository) is what holds
// the merge on CodeCommit.
func (codecommitProvider) PublishCheck(ctx context.Context, c *ChangeClient, repo RepoRef, sha string, res CheckResult) error {
	if res.ChangeID == "" {
		return errors.New("codecommit gates pull requests: the change is required")
	}
	cl, _, err := c.ccClient(ctx, repo)
	if err != nil {
		return err
	}
	var pr *cctypes.PullRequest
	if err := c.ccCall(ctx, func() error {
		out, err := cl.GetPullRequest(ctx, &codecommit.GetPullRequestInput{PullRequestId: aws.String(res.ChangeID)})
		if err == nil {
			pr = out.PullRequest
		}
		return err
	}); err != nil {
		return err
	}
	if len(pr.PullRequestTargets) == 0 || aws.ToString(pr.PullRequestTargets[0].SourceCommit) != sha {
		return nil // the change moved on; the next verdict is for its new head
	}
	state := cctypes.ApprovalStateRevoke
	if res.State == "success" {
		state = cctypes.ApprovalStateApprove
	}
	return c.ccCall(ctx, func() error {
		_, err := cl.UpdatePullRequestApprovalState(ctx, &codecommit.UpdatePullRequestApprovalStateInput{
			PullRequestId: aws.String(res.ChangeID), RevisionId: pr.RevisionId, ApprovalState: state})
		return err
	})
}

// Merge merges only the expected head (CodeCommit checks sourceCommitId).
func (codecommitProvider) Merge(ctx context.Context, c *ChangeClient, repo RepoRef, id string, opts MergeOpts) (string, error) {
	cl, r, err := c.ccClient(ctx, repo)
	if err != nil {
		return "", err
	}
	var merged *cctypes.PullRequest
	err = c.ccCall(ctx, func() error {
		var err error
		switch opts.Method {
		case "squash":
			var o *codecommit.MergePullRequestBySquashOutput
			o, err = cl.MergePullRequestBySquash(ctx, &codecommit.MergePullRequestBySquashInput{PullRequestId: aws.String(id),
				RepositoryName: aws.String(r.Name), SourceCommitId: aws.String(opts.ExpectedHead)})
			if err == nil {
				merged = o.PullRequest
			}
		case "fast_forward":
			var o *codecommit.MergePullRequestByFastForwardOutput
			o, err = cl.MergePullRequestByFastForward(ctx, &codecommit.MergePullRequestByFastForwardInput{PullRequestId: aws.String(id),
				RepositoryName: aws.String(r.Name), SourceCommitId: aws.String(opts.ExpectedHead)})
			if err == nil {
				merged = o.PullRequest
			}
		default:
			var o *codecommit.MergePullRequestByThreeWayOutput
			o, err = cl.MergePullRequestByThreeWay(ctx, &codecommit.MergePullRequestByThreeWayInput{PullRequestId: aws.String(id),
				RepositoryName: aws.String(r.Name), SourceCommitId: aws.String(opts.ExpectedHead)})
			if err == nil {
				merged = o.PullRequest
			}
		}
		return err
	})
	var se *StatusError
	if errors.As(err, &se) {
		switch se.Message {
		case "TipOfSourceReferenceIsDifferentException":
			return "", ErrHeadMoved
		case "PullRequestApprovalRulesNotSatisfiedException", "ManualMergeRequiredException",
			"PullRequestAlreadyClosedException", "TipsDivergenceExceededException":
			return "", fmt.Errorf("%w: %s", ErrNotMergeable, se.Message)
		}
	}
	if err != nil {
		return "", err
	}
	if merged != nil && len(merged.PullRequestTargets) > 0 && merged.PullRequestTargets[0].MergeMetadata != nil {
		return aws.ToString(merged.PullRequestTargets[0].MergeMetadata.MergeCommitId), nil
	}
	return "", nil
}

// PostComment posts with CodeCommit's client request token set to Forge's
// marker id, so a retried post is the same comment.
func (codecommitProvider) PostComment(ctx context.Context, c *ChangeClient, repo RepoRef, id string, req CommentRequest) (string, error) {
	cl, r, err := c.ccClient(ctx, repo)
	if err != nil {
		return "", err
	}
	_, uid := Unmark(req.Body)
	var token *string
	if uid != "" {
		token = aws.String(uid)
	}
	if req.ReplyTo != "" {
		var cid string
		err := c.ccCall(ctx, func() error {
			out, err := cl.PostCommentReply(ctx, &codecommit.PostCommentReplyInput{InReplyTo: aws.String(req.ReplyTo),
				Content: aws.String(req.Body), ClientRequestToken: token})
			if err == nil && out.Comment != nil {
				cid = aws.ToString(out.Comment.CommentId)
			}
			return err
		})
		return cid, err
	}
	var pr *cctypes.PullRequest
	if err := c.ccCall(ctx, func() error {
		out, err := cl.GetPullRequest(ctx, &codecommit.GetPullRequestInput{PullRequestId: aws.String(id)})
		if err == nil {
			pr = out.PullRequest
		}
		return err
	}); err != nil {
		return "", err
	}
	if len(pr.PullRequestTargets) == 0 {
		return "", ErrChangeNotFound
	}
	t := pr.PullRequestTargets[0]
	in := &codecommit.PostCommentForPullRequestInput{PullRequestId: aws.String(id), RepositoryName: aws.String(r.Name),
		BeforeCommitId: t.DestinationCommit, AfterCommitId: t.SourceCommit, Content: aws.String(req.Body), ClientRequestToken: token}
	if req.Anchor != nil {
		version := cctypes.RelativeFileVersionEnumAfter
		if req.Anchor.Side == "old" {
			version = cctypes.RelativeFileVersionEnumBefore
		}
		in.Location = &cctypes.Location{FilePath: aws.String(req.Anchor.Path), FilePosition: aws.Int64(int64(req.Anchor.Line)),
			RelativeFileVersion: version}
	}
	var cid string
	err = c.ccCall(ctx, func() error {
		out, err := cl.PostCommentForPullRequest(ctx, in)
		if err == nil && out.Comment != nil {
			cid = aws.ToString(out.Comment.CommentId)
		}
		return err
	})
	return cid, err
}

func (codecommitProvider) ResolveThread(context.Context, *ChangeClient, RepoRef, string, string, bool) error {
	return ErrNoResolve
}

// PostReview: approvals on CodeCommit are each person's own IAM identity,
// which Forge does not hold; their verdict counts in Forge's gate.
func (codecommitProvider) PostReview(context.Context, *ChangeClient, RepoRef, string, string, string, string) (string, error) {
	return "", &StatusError{Provider: "codecommit", StatusCode: http.StatusForbidden, Message: "reviews are made with each person's own IAM identity"}
}
