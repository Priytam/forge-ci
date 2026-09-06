package api

// AWS CodeCommit has no webhooks. Repository events are published to
// EventBridge, and an EventBridge *API Destination* forwards them here as an
// HTTP POST carrying the whole event envelope (not just the detail block) — so
// the envelope `id` is available as the delivery-dedup key, exactly like
// GitHub's X-GitHub-Delivery header. The deployment side wires the EventBridge
// rule; see docs/codecommit.md.
//
// Two event families matter:
//
//	CodeCommit Repository State Change   -> referenceCreated / referenceUpdated -> push
//	CodeCommit Pull Request State Change -> pullRequestCreated /
//	                                        pullRequestSourceBranchUpdated      -> merge_request
//
// Everything else (branch deletions, PR status/merge changes) is acknowledged
// with 202 and ignored, mirroring how the GitHub handler ignores non-triggering
// pull_request actions.

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/priytamjeepandey/forge-ci/internal/compiler"
)

// CodeCommit event kinds Forge acts on. ccKindIgnore ("") is every other event.
const (
	ccKindIgnore      = ""
	ccKindPush        = "push"
	ccKindPullRequest = "pull_request"
)

// codeCommitEnvelope is the EventBridge envelope subset Forge reads. `id` is the
// per-event unique id EventBridge assigns — the dedup key that makes a
// redelivered event a no-op. `detail-type` is a coarse discriminator used only
// when `detail.event` is unrecognized.
type codeCommitEnvelope struct {
	ID         string          `json:"id"`
	DetailType string          `json:"detail-type"`
	Detail     json.RawMessage `json:"detail"`
}

// ccPush is a normalized CodeCommit reference-change event: the push subset of
// {repo, ref, sha} plus a best-effort author.
type ccPush struct {
	Repo   string // CodeCommit repository name (the Forge repo key)
	Ref    string // branch/tag short name (refs/heads/ and refs/tags/ stripped)
	SHA    string // the commit the reference now points at
	Author string // IAM principal that pushed, when the event carries one
}

// codeCommitEventID returns the EventBridge envelope id used to dedup
// redeliveries. It returns "" when the payload carries none, so the caller can
// fall back to a body hash the way the GitHub and Bitbucket handlers do.
func codeCommitEventID(body []byte) string {
	var env codeCommitEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return ""
	}
	return env.ID
}

// codeCommitEventKind classifies an EventBridge payload into the kind of
// pipeline it should trigger, or ccKindIgnore.
//
// `detail.event` is the authoritative discriminator and wins whenever it is
// recognized; `detail-type` is only consulted for an unrecognized event name so
// a new AWS event name in a known family still routes sensibly. Both the real
// AWS event names (`pullRequestSourceBranchUpdated`) and the abbreviated names
// used in Forge's requirement doc (`pullRequestSourceUpdated`, `SourceUpdated`)
// are accepted, since deployments differ in which they forward.
func codeCommitEventKind(body []byte) string {
	var env codeCommitEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return ccKindIgnore
	}
	var detail struct {
		Event string `json:"event"`
	}
	_ = json.Unmarshal(env.Detail, &detail)

	switch detail.Event {
	case "referenceCreated", "referenceUpdated":
		return ccKindPush
	case "referenceDeleted":
		// A deleted branch has no commit to build.
		return ccKindIgnore
	case "pullRequestCreated", "pullRequestSourceBranchUpdated",
		"pullRequestSourceUpdated", "SourceUpdated":
		return ccKindPullRequest
	case "pullRequestStatusChanged", "pullRequestMergeStatusUpdated":
		// Closed / merged — the GitHub path ignores these actions too.
		return ccKindIgnore
	}

	// Unrecognized (or absent) detail.event: fall back to the event family.
	switch env.DetailType {
	case "CodeCommit Repository State Change", "Reference Changes":
		return ccKindPush
	case "CodeCommit Pull Request State Change", "Pull Request State Change":
		return ccKindPullRequest
	}
	return ccKindIgnore
}

// parseCodeCommitPush extracts a ccPush from a CodeCommit Repository State
// Change payload. A reference change with no commitId (or the all-zero sha a
// deletion carries) yields an empty SHA, which the caller treats as nothing to
// build.
func parseCodeCommitPush(body []byte) (ccPush, error) {
	var env codeCommitEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return ccPush{}, err
	}
	var detail struct {
		RepositoryName string `json:"repositoryName"`
		ReferenceName  string `json:"referenceName"`
		ReferenceType  string `json:"referenceType"`
		CommitID       string `json:"commitId"`
		CallerUserARN  string `json:"callerUserArn"`
	}
	if err := json.Unmarshal(env.Detail, &detail); err != nil {
		return ccPush{}, err
	}
	sha := detail.CommitID
	if isZeroSHA(sha) {
		sha = ""
	}
	return ccPush{
		Repo:   detail.RepositoryName,
		Ref:    shortRefName(detail.ReferenceName),
		SHA:    sha,
		Author: arnPrincipal(detail.CallerUserARN),
	}, nil
}

// parseCodeCommitPR extracts a prTrigger from a CodeCommit Pull Request State
// Change payload, so CodeCommit PRs join the same merge_request funnel as the
// GitHub and Bitbucket PR paths, plus the prSource that lets Forge comment back
// on the PR later.
//
// Two shape differences from the other providers are normalized here:
// CodeCommit names the repo in a `repositoryNames` ARRAY (a PR can span
// repositories; Forge builds the first, which is the source repo), and
// `pullRequestId` is a STRING where prTrigger.IID is an int — so the raw string
// id is carried separately in the prSource rather than widening the shared
// prTrigger the other two providers fill in. Action is left empty —
// codeCommitEventKind has already gated which events trigger a build, the same
// division of labour as the Bitbucket handler.
func parseCodeCommitPR(body []byte) (prTrigger, prSource, error) {
	var env codeCommitEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return prTrigger{}, prSource{}, err
	}
	var detail struct {
		RepositoryNames      []string `json:"repositoryNames"`
		RepositoryName       string   `json:"repositoryName"`
		PullRequestID        string   `json:"pullRequestId"`
		Title                string   `json:"title"`
		SourceReference      string   `json:"sourceReference"`
		DestinationReference string   `json:"destinationReference"`
		SourceCommit         string   `json:"sourceCommit"`
		DestinationCommit    string   `json:"destinationCommit"`
		Author               string   `json:"author"`
	}
	if err := json.Unmarshal(env.Detail, &detail); err != nil {
		return prTrigger{}, prSource{}, err
	}
	repo := detail.RepositoryName
	if len(detail.RepositoryNames) > 0 {
		repo = detail.RepositoryNames[0]
	}
	iid, _ := strconv.Atoi(detail.PullRequestID) // non-numeric ids degrade to 0
	return prTrigger{
		Repo:         repo,
		IID:          iid,
		Title:        detail.Title,
		SourceBranch: shortRefName(detail.SourceReference),
		TargetBranch: shortRefName(detail.DestinationReference),
		HeadSHA:      detail.SourceCommit,
		Author:       arnPrincipal(detail.Author),
	}, prSource{
		IID:     detail.PullRequestID,
		BaseSHA: detail.DestinationCommit,
	}, nil
}

// shortRefName strips the refs/heads/ or refs/tags/ prefix CodeCommit uses in
// referenceName / sourceReference, matching the bare branch names the GitHub and
// Bitbucket paths hand the compiler.
func shortRefName(ref string) string {
	ref = strings.TrimPrefix(ref, "refs/heads/")
	return strings.TrimPrefix(ref, "refs/tags/")
}

// isZeroSHA reports whether sha is git's all-zero object id, which a reference
// deletion carries in place of a real commit.
func isZeroSHA(sha string) bool {
	if sha == "" {
		return false
	}
	return strings.Trim(sha, "0") == ""
}

// arnPrincipal reduces an IAM ARN to the human-meaningful trailing segment so
// the pipeline's triggered_by reads like the other providers' usernames:
//
//	arn:aws:iam::123456789012:user/alice              -> alice
//	arn:aws:sts::123456789012:assumed-role/Dev/alice  -> alice
//
// A value that is not an ARN is passed through unchanged, and "" stays "".
func arnPrincipal(arn string) string {
	if !strings.HasPrefix(arn, "arn:") {
		return arn
	}
	if i := strings.LastIndex(arn, "/"); i >= 0 && i < len(arn)-1 {
		return arn[i+1:]
	}
	// No path component (e.g. arn:aws:iam::123:root) — use the resource part.
	if i := strings.LastIndex(arn, ":"); i >= 0 && i < len(arn)-1 {
		return arn[i+1:]
	}
	return arn
}

// codecommitWebhook handles EventBridge deliveries for CodeCommit repositories.
// Unlike GitHub and Bitbucket the event kind is carried in the BODY rather than
// a header, so the payload is classified before dispatch. Unknown events are
// acknowledged (202) and ignored.
func (s *Server) codecommitWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "unreadable body")
		return
	}
	switch codeCommitEventKind(body) {
	case ccKindPush:
		s.codecommitPush(w, r, body)
	case ccKindPullRequest:
		s.codecommitPullRequest(w, r, body)
	default:
		w.WriteHeader(http.StatusAccepted) // ignore other events politely
	}
}

// codecommitPush creates a pipeline for a reference change at the new commit.
func (s *Server) codecommitPush(w http.ResponseWriter, r *http.Request, body []byte) {
	push, err := parseCodeCommitPush(body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}
	if push.SHA == "" {
		w.WriteHeader(http.StatusAccepted) // deletion / no commit to build
		return
	}
	// Dedup: EventBridge assigns every event a unique id; fall back to a hash of
	// the body so a redelivery without one still dedups.
	deliveryID := codeCommitEventID(body)
	if deliveryID == "" {
		deliveryID = bodyHash(body)
	}
	proceed, created := s.dedupDelivery(w, r, "codecommit", deliveryID)
	if !proceed {
		return
	}
	defer func() {
		if !*created {
			_ = s.store.ForgetWebhookDelivery(r.Context(), "codecommit", deliveryID)
		}
	}()
	// CodeCommit's EventBridge payload carries no commit author or message —
	// only the commit id — so both stay empty rather than being guessed at.
	*created = s.triggerFromWebhook(w, r, webhookRun{
		Repo: push.Repo, Ref: push.Ref, SHA: push.SHA,
		Actor: push.Author, Source: compiler.SourceWebhook,
	})
}

// codecommitPullRequest creates a merge_request pipeline for a CodeCommit PR
// event, built on the PR source commit with the source branch as the compile ref
// — the same head-sha / branch-ref semantics as the GitHub and Bitbucket PR
// paths, so branch-keyed only/except and rules behave identically.
// CI_PIPELINE_SOURCE=merge_request and the CI_MERGE_REQUEST_* vars are threaded
// into rules and job env.
func (s *Server) codecommitPullRequest(w http.ResponseWriter, r *http.Request, body []byte) {
	pr, src, err := parseCodeCommitPR(body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}
	deliveryID := codeCommitEventID(body)
	if deliveryID == "" {
		deliveryID = bodyHash(body)
	}
	proceed, created := s.dedupDelivery(w, r, "codecommit", deliveryID)
	if !proceed {
		return
	}
	defer func() {
		if !*created {
			_ = s.store.ForgetWebhookDelivery(r.Context(), "codecommit", deliveryID)
		}
	}()
	ctx := mergeRequestContext(pr.IID, pr.SourceBranch, pr.TargetBranch, pr.Title)
	// CodeCommit has no commit-status API, so the PR id and the destination
	// commit are recorded now — they are the only way to comment on this PR once
	// the run finishes.
	*created = s.triggerFromWebhook(w, r, webhookRun{
		Repo: pr.Repo, Ref: pr.SourceBranch, SHA: pr.HeadSHA,
		Actor: pr.Author, Source: compiler.SourceMergeRequest,
		CommitMessage: pr.Title, ExtraCtx: ctx, PR: &src,
	})
}
