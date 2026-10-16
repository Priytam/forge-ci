package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/priytamjeepandey/forge-ci/internal/compiler"
)

// GitLab (gitlab.com or self-hosted) and Gitea/Forgejo webhooks. Both feed
// the same trigger path as every other host (triggerFromWebhook), so config
// resolution, rules, change sets and dedup behave identically.

const zeroSHA = "0000000000000000000000000000000000000000"

// withDelivery runs fn once per delivery id (dedup, with rollback when no
// pipeline was created), exactly like the GitHub handlers.
func (s *Server) withDelivery(w http.ResponseWriter, r *http.Request, provider, deliveryID string, body []byte, fn func() bool) {
	if deliveryID == "" {
		deliveryID = bodyHash(body)
	}
	proceed, created := s.dedupDelivery(w, r, provider, deliveryID)
	if !proceed {
		return
	}
	defer func() {
		if !*created {
			_ = s.store.ForgetWebhookDelivery(r.Context(), provider, deliveryID)
		}
	}()
	*created = fn()
}

// ---- GitLab ----

// gitlabWebhook handles Push Hook, Tag Push Hook and Merge Request Hook. When
// WEBHOOK_SECRET is set it must equal the X-Gitlab-Token header (GitLab sends
// the secret verbatim rather than signing the body).
func (s *Server) gitlabWebhook(w http.ResponseWriter, r *http.Request) {
	body, ok := s.readWebhookBody(w, r)
	if !ok {
		return
	}
	if secret := os.Getenv("WEBHOOK_SECRET"); secret != "" {
		if subtle.ConstantTimeCompare([]byte(secret), []byte(r.Header.Get("X-Gitlab-Token"))) != 1 {
			writeErr(w, http.StatusUnauthorized, "invalid webhook token")
			return
		}
	}
	delivery := r.Header.Get("X-Gitlab-Event-UUID")
	if delivery == "" {
		delivery = r.Header.Get("Idempotency-Key")
	}
	switch r.Header.Get("X-Gitlab-Event") {
	case "Push Hook", "Tag Push Hook":
		run, skip, err := parseGitLabPush(body)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid JSON payload")
			return
		}
		if skip {
			w.WriteHeader(http.StatusAccepted) // branch/tag deletion
			return
		}
		s.withDelivery(w, r, "gitlab", delivery, body, func() bool { return s.triggerFromWebhook(w, r, run) })
	case "Merge Request Hook":
		pr, trigger, err := parseGitLabMR(body)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid JSON payload")
			return
		}
		if !trigger {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		s.withDelivery(w, r, "gitlab", delivery, body, func() bool { return s.triggerMergeRequest(w, r, pr) })
	default:
		w.WriteHeader(http.StatusAccepted)
	}
}

func parseGitLabPush(body []byte) (run webhookRun, skip bool, err error) {
	var p struct {
		ObjectKind   string `json:"object_kind"`
		Ref          string `json:"ref"`
		After        string `json:"after"`
		CheckoutSHA  string `json:"checkout_sha"`
		UserUsername string `json:"user_username"`
		UserName     string `json:"user_name"`
		Project      struct {
			PathWithNamespace string `json:"path_with_namespace"`
		} `json:"project"`
		Commits []struct {
			ID      string `json:"id"`
			Message string `json:"message"`
			Author  struct {
				Name string `json:"name"`
			} `json:"author"`
		} `json:"commits"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return webhookRun{}, false, err
	}
	sha := p.CheckoutSHA
	if sha == "" {
		sha = p.After
	}
	if sha == "" || sha == zeroSHA {
		return webhookRun{}, true, nil
	}
	actor := p.UserUsername
	if actor == "" {
		actor = p.UserName
	}
	var author, message string
	for _, c := range p.Commits {
		if c.ID == sha {
			author, message = c.Author.Name, c.Message
		}
	}
	isTag := strings.HasPrefix(p.Ref, "refs/tags/") || p.ObjectKind == "tag_push"
	ref := strings.TrimPrefix(strings.TrimPrefix(p.Ref, "refs/heads/"), "refs/tags/")
	run = webhookRun{
		Repo: p.Project.PathWithNamespace, Ref: ref, SHA: sha, Actor: actor,
		Source: compiler.SourcePush, CommitAuthor: author, CommitMessage: message,
	}
	run.ExtraCtx = tagContext(isTag, ref)
	return run, false, nil
}

// parseGitLabMR builds a merge-request trigger. Opening, reopening and new
// commits (an "update" carrying oldrev) build; title edits, approvals, merges
// and closes don't.
func parseGitLabMR(body []byte) (prTrigger, bool, error) {
	var p struct {
		User struct {
			Username string `json:"username"`
		} `json:"user"`
		Project struct {
			PathWithNamespace string `json:"path_with_namespace"`
		} `json:"project"`
		Attrs struct {
			IID          int    `json:"iid"`
			Title        string `json:"title"`
			SourceBranch string `json:"source_branch"`
			TargetBranch string `json:"target_branch"`
			Action       string `json:"action"`
			OldRev       string `json:"oldrev"`
			LastCommit   struct {
				ID string `json:"id"`
			} `json:"last_commit"`
		} `json:"object_attributes"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return prTrigger{}, false, err
	}
	a := p.Attrs
	trigger := a.Action == "open" || a.Action == "reopen" || (a.Action == "update" && a.OldRev != "")
	return prTrigger{
		Action: a.Action, Repo: p.Project.PathWithNamespace, IID: a.IID, Title: a.Title,
		SourceBranch: a.SourceBranch, TargetBranch: a.TargetBranch, HeadSHA: a.LastCommit.ID,
		Author: p.User.Username,
	}, trigger && a.LastCommit.ID != "", nil
}

// triggerMergeRequest creates a merge_request pipeline from a normalized
// trigger (shared by GitLab and Gitea).
func (s *Server) triggerMergeRequest(w http.ResponseWriter, r *http.Request, pr prTrigger) bool {
	return s.triggerFromWebhook(w, r, webhookRun{
		Repo: pr.Repo, Ref: pr.SourceBranch, SHA: pr.HeadSHA,
		Actor: pr.Author, Source: compiler.SourceMergeRequest,
		CommitMessage: pr.Title,
		ExtraCtx:      mergeRequestContext(pr.IID, pr.SourceBranch, pr.TargetBranch, pr.Title),
		PR:            &prSource{IID: strconv.Itoa(pr.IID)},
	})
}

// ---- Gitea / Forgejo ----

// giteaWebhook handles push and pull_request events from Gitea or Forgejo.
// When WEBHOOK_SECRET is set the body's HMAC-SHA256 (hex) must match
// X-Gitea-Signature (or X-Forgejo-Signature / X-Gogs-Signature).
func (s *Server) giteaWebhook(w http.ResponseWriter, r *http.Request) {
	body, ok := s.readWebhookBody(w, r)
	if !ok {
		return
	}
	header := func(name string) string {
		for _, prefix := range []string{"X-Gitea-", "X-Forgejo-", "X-Gogs-"} {
			if v := r.Header.Get(prefix + name); v != "" {
				return v
			}
		}
		return ""
	}
	if secret := os.Getenv("WEBHOOK_SECRET"); secret != "" {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		want := hex.EncodeToString(mac.Sum(nil))
		if !hmac.Equal([]byte(want), []byte(strings.ToLower(header("Signature")))) {
			writeErr(w, http.StatusUnauthorized, "invalid webhook signature")
			return
		}
	}
	delivery := header("Delivery")
	switch header("Event") {
	case "push":
		run, skip, err := parseGiteaPush(body)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid JSON payload")
			return
		}
		if skip {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		s.withDelivery(w, r, "gitea", delivery, body, func() bool { return s.triggerFromWebhook(w, r, run) })
	case "pull_request":
		pr, err := parseGiteaPR(body)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid JSON payload")
			return
		}
		if pr.Action != "opened" && pr.Action != "reopened" && pr.Action != "synchronized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		s.withDelivery(w, r, "gitea", delivery, body, func() bool { return s.triggerMergeRequest(w, r, pr) })
	default:
		w.WriteHeader(http.StatusAccepted)
	}
}

func parseGiteaPush(body []byte) (webhookRun, bool, error) {
	var p struct {
		Ref    string `json:"ref"`
		After  string `json:"after"`
		Pusher struct {
			Login string `json:"login"`
		} `json:"pusher"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		HeadCommit *struct {
			Message string `json:"message"`
			Author  struct {
				Name     string `json:"name"`
				Username string `json:"username"`
			} `json:"author"`
		} `json:"head_commit"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return webhookRun{}, false, err
	}
	if p.After == "" || p.After == zeroSHA {
		return webhookRun{}, true, nil
	}
	isTag := strings.HasPrefix(p.Ref, "refs/tags/")
	ref := strings.TrimPrefix(strings.TrimPrefix(p.Ref, "refs/heads/"), "refs/tags/")
	run := webhookRun{Repo: p.Repository.FullName, Ref: ref, SHA: p.After, Actor: p.Pusher.Login, Source: compiler.SourcePush}
	if c := p.HeadCommit; c != nil {
		run.CommitAuthor = c.Author.Username
		if run.CommitAuthor == "" {
			run.CommitAuthor = c.Author.Name
		}
		run.CommitMessage = c.Message
	}
	run.ExtraCtx = tagContext(isTag, ref)
	return run, false, nil
}

func parseGiteaPR(body []byte) (prTrigger, error) {
	var p struct {
		Action      string `json:"action"`
		Number      int    `json:"number"`
		PullRequest struct {
			Title string `json:"title"`
			Head  struct {
				Ref string `json:"ref"`
				SHA string `json:"sha"`
			} `json:"head"`
			Base struct {
				Ref string `json:"ref"`
			} `json:"base"`
			User struct {
				Login string `json:"login"`
			} `json:"user"`
		} `json:"pull_request"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return prTrigger{}, err
	}
	pr := p.PullRequest
	return prTrigger{
		Action: p.Action, Repo: p.Repository.FullName, IID: p.Number, Title: pr.Title,
		SourceBranch: pr.Head.Ref, TargetBranch: pr.Base.Ref, HeadSHA: pr.Head.SHA, Author: pr.User.Login,
	}, nil
}
