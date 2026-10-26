package vcstest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/priytamjeepandey/forge-ci/internal/vcs"
)

// FakeForgeARN is the AWS identity Forge's control plane calls CodeCommit as.
const FakeForgeARN = "arn:aws:sts::111122223333:assumed-role/forge-control-plane/forge"

// FakeCodeCommit is CodeCommit's JSON-1.1 API for one repository ("shop")
// over the shared core. Every call is made as Forge's identity; Forge's
// approval on a pull request plays the merge gate (an approval rule that
// requires it holds the merge).
type FakeCodeCommit struct {
	*core
	Server *httptest.Server
	tokens map[string]int64 // client request token → comment id
}

// NewFakeCodeCommit starts the fake and points CODECOMMIT_API_BASE at it.
func NewFakeCodeCommit(t *testing.T) *FakeCodeCommit {
	t.Helper()
	f := &FakeCodeCommit{core: newCore("forge", false), tokens: map[string]int64{}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Server.Close)
	t.Setenv("CODECOMMIT_API_BASE", f.Server.URL)
	t.Setenv("CODECOMMIT_FORGE_ARN", FakeForgeARN)
	return f
}

func (f *FakeCodeCommit) Repo() vcs.RepoRef {
	return vcs.RepoRef{Provider: "codecommit", Repo: "shop", CloneURL: "codecommit::us-east-1://shop", Region: "us-east-1"}
}

func userARN(login string) string {
	if login == "forge" {
		return FakeForgeARN
	}
	return "arn:aws:iam::111122223333:user/" + login
}

func (f *FakeCodeCommit) Delivery(t *testing.T, id, kind string) (http.Header, []byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pull(t, id)
	f.delivery++
	detailType, event := "CodeCommit Pull Request State Change", ""
	switch kind {
	case "change":
		event = "pullRequestSourceBranchUpdated"
	case "review":
		event = "pullRequestApprovalStateChanged"
	case "comment":
		detailType, event = "CodeCommit Comment on Pull Request", "commentOnPullRequestCreated"
	default:
		return nil, nil, false
	}
	body, _ := json.Marshal(map[string]any{"id": "ev-" + strconv.Itoa(f.delivery), "source": "aws.codecommit",
		"detail-type": detailType, "detail": map[string]any{"event": event, "pullRequestId": id, "repositoryNames": []string{"shop"}}})
	return http.Header{}, body, true
}

func (f *FakeCodeCommit) Unrelated(*testing.T) (http.Header, []byte, bool) {
	return http.Header{}, []byte(`{"id":"ev-push","detail-type":"CodeCommit Repository State Change",` +
		`"detail":{"event":"referenceUpdated","repositoryName":"shop","referenceName":"main","commitId":"abc"}}`), true
}

func epoch(t interface{ Unix() int64 }) float64 { return float64(t.Unix()) }

func (f *FakeCodeCommit) prJSON(p *cPull) map[string]any {
	status := "OPEN"
	if p.state != "open" {
		status = "CLOSED"
	}
	return map[string]any{"pullRequestId": strconv.Itoa(p.number), "title": p.title, "description": p.body,
		"pullRequestStatus": status, "authorArn": userARN(p.author), "creationDate": epoch(p.created),
		"lastActivityDate": epoch(p.updated), "revisionId": "rev-" + p.head,
		"pullRequestTargets": []any{map[string]any{"repositoryName": "shop", "sourceReference": "refs/heads/" + p.headRef,
			"destinationReference": "refs/heads/main", "sourceCommit": p.head, "destinationCommit": p.base,
			"mergeMetadata": map[string]any{"isMerged": p.state == "merged"}}}}
}

func ccCommentJSON(c cComment, inReplyTo int64) map[string]any {
	m := map[string]any{"commentId": strconv.FormatInt(c.id, 10), "content": c.body, "authorArn": userARN(c.login),
		"creationDate": epoch(c.at), "lastModifiedDate": epoch(c.at), "deleted": false}
	if inReplyTo != 0 {
		m["inReplyTo"] = strconv.FormatInt(inReplyTo, 10)
	}
	return m
}

func (f *FakeCodeCommit) fault(w http.ResponseWriter, code int, typ string) {
	w.Header().Set("X-Amzn-Errortype", typ)
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(`{"__type":"` + typ + `","message":"` + typ + `"}`))
}

func (f *FakeCodeCommit) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	if f.reject != nil {
		f.reject = nil
		f.fault(w, http.StatusBadRequest, "ThrottlingException")
		return
	}
	op := strings.TrimPrefix(r.Header.Get("X-Amz-Target"), "CodeCommit_20150413.")
	var in map[string]any
	_ = json.NewDecoder(r.Body).Decode(&in)
	str := func(k string) string { v, _ := in[k].(string); return v }
	reply := func(v any) {
		if strings.HasPrefix(op, "Post") || strings.HasPrefix(op, "Merge") || strings.HasPrefix(op, "Update") {
			if f.loseNext {
				f.loseNext = false
				f.fault(w, http.StatusInternalServerError, "InternalFailure")
				return
			}
		}
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		_ = json.NewEncoder(w).Encode(v)
	}
	if op == "ListPullRequests" {
		ids := []string{}
		for _, p := range f.open() {
			ids = append(ids, strconv.Itoa(p.number))
		}
		reply(map[string]any{"pullRequestIds": ids})
		return
	}
	if op == "PostCommentReply" {
		if prev, ok := f.tokens[str("clientRequestToken")]; ok && str("clientRequestToken") != "" {
			reply(map[string]any{"comment": map[string]any{"commentId": strconv.FormatInt(prev, 10)}})
			return
		}
		for _, p := range f.pulls {
			if th := f.threadByID(p, str("inReplyTo")); th != nil {
				c := f.reply(p, th, f.bot, str("content"))
				f.tokens[str("clientRequestToken")] = c.id
				reply(map[string]any{"comment": ccCommentJSON(c, 0)})
				return
			}
		}
		f.fault(w, http.StatusBadRequest, "CommentDoesNotExistException")
		return
	}
	n, _ := strconv.Atoi(str("pullRequestId"))
	p, ok := f.pulls[n]
	if !ok {
		f.fault(w, http.StatusBadRequest, "PullRequestDoesNotExistException")
		return
	}
	switch op {
	case "GetPullRequest":
		reply(map[string]any{"pullRequest": f.prJSON(p)})
	case "GetPullRequestApprovalStates":
		approvals := []any{}
		for login := range p.approved {
			approvals = append(approvals, map[string]any{"userArn": userARN(login), "approvalState": "APPROVE"})
		}
		reply(map[string]any{"approvals": approvals})
	case "GetCommentsForPullRequest":
		data := []any{}
		for _, th := range p.threads {
			version := "AFTER"
			if th.side == "old" {
				version = "BEFORE"
			}
			var cs []any
			for i, c := range th.comments {
				parent := int64(0)
				if i > 0 {
					parent = th.comments[0].id
				}
				cs = append(cs, ccCommentJSON(c, parent))
			}
			data = append(data, map[string]any{"beforeCommitId": p.base, "afterCommitId": th.commit,
				"location": map[string]any{"filePath": th.path, "filePosition": th.line, "relativeFileVersion": version}, "comments": cs})
		}
		if len(p.convo) > 0 {
			var cs []any
			for _, c := range p.convo {
				cs = append(cs, ccCommentJSON(c, 0))
			}
			data = append(data, map[string]any{"beforeCommitId": p.base, "afterCommitId": p.head, "comments": cs})
		}
		reply(map[string]any{"commentsForPullRequestData": data})
	case "UpdatePullRequestApprovalState":
		if str("revisionId") != "rev-"+p.head {
			f.fault(w, http.StatusBadRequest, "RevisionNotCurrentException")
			return
		}
		// Forge's approval is the gate (an approval rule requires it).
		state := "pending"
		if str("approvalState") == "APPROVE" {
			state = "success"
		}
		f.setStatus(p.head, vcs.GateContext, state)
		reply(map[string]any{})
	case "MergePullRequestByThreeWay", "MergePullRequestBySquash", "MergePullRequestByFastForward":
		if src := str("sourceCommitId"); src != "" && src != p.head {
			f.fault(w, http.StatusBadRequest, "TipOfSourceReferenceIsDifferentException")
			return
		}
		if msg := f.blocked(p); msg != "" {
			f.fault(w, http.StatusBadRequest, "PullRequestApprovalRulesNotSatisfiedException")
			return
		}
		p.state = "merged"
		f.touch(p)
		reply(map[string]any{"pullRequest": f.prJSON(p)})
	case "PostCommentForPullRequest":
		if prev, ok := f.tokens[str("clientRequestToken")]; ok && str("clientRequestToken") != "" {
			reply(map[string]any{"comment": map[string]any{"commentId": strconv.FormatInt(prev, 10)}})
			return
		}
		var c cComment
		if loc, ok := in["location"].(map[string]any); ok {
			side := "new"
			if loc["relativeFileVersion"] == "BEFORE" {
				side = "old"
			}
			_, c = f.addThread(p, f.bot, loc["filePath"].(string), side, int(loc["filePosition"].(float64)), str("afterCommitId"), str("content"))
		} else {
			c = f.addConvo(p, f.bot, str("content"))
		}
		f.tokens[str("clientRequestToken")] = c.id
		reply(map[string]any{"comment": ccCommentJSON(c, 0)})
	default:
		f.fault(w, http.StatusBadRequest, "UnknownOperationException")
	}
}
