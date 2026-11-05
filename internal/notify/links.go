package notify

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/secret"
)

// Signed approve/reject links let an approver act from an email without
// signing in first. A link names the job, the approver and the verdict, and
// expires; it is signed with a key derived from FORGE_SECRET_KEY, so links
// exist only on servers with that key. Acting still goes through the normal
// approval rules (role, self-approval, duplicate votes) as that approver.

const linkPurpose = "approval-link"

// ApprovalLinkTTL is how long a link works.
const ApprovalLinkTTL = 24 * time.Hour

type linkClaims struct {
	JobID    int64  `json:"j"`
	Approver string `json:"a"`
	Verdict  string `json:"v"`
	Exp      int64  `json:"e"`
}

// SignApprovalLink returns the token for one approver's verdict on a job, or
// "" when signing is unavailable (no secret key).
func SignApprovalLink(c *secret.Cipher, jobID int64, approver, verdict string, exp time.Time) string {
	if !c.HasKey() {
		return ""
	}
	payload, _ := json.Marshal(linkClaims{JobID: jobID, Approver: strings.ToLower(approver), Verdict: verdict, Exp: exp.Unix()})
	sig := c.Sign(linkPurpose, payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// ParseApprovalLink verifies a token and returns its claims.
func ParseApprovalLink(c *secret.Cipher, token string) (jobID int64, approver, verdict string, err error) {
	p, s, ok := strings.Cut(token, ".")
	if !ok {
		return 0, "", "", errors.New("malformed link")
	}
	payload, err1 := base64.RawURLEncoding.DecodeString(p)
	sig, err2 := base64.RawURLEncoding.DecodeString(s)
	if err1 != nil || err2 != nil || !c.Verify(linkPurpose, payload, sig) {
		return 0, "", "", errors.New("this link is not valid")
	}
	var cl linkClaims
	if err := json.Unmarshal(payload, &cl); err != nil {
		return 0, "", "", errors.New("malformed link")
	}
	if time.Now().Unix() > cl.Exp {
		return 0, "", "", errors.New("this link has expired; approve from the job page instead")
	}
	if cl.Verdict != "approved" && cl.Verdict != "rejected" {
		return 0, "", "", errors.New("malformed link")
	}
	return cl.JobID, cl.Approver, cl.Verdict, nil
}

// ApprovalLinksFor builds per-approver links for ev (an approval_needed
// event) and stores them in ev.Actions. apiBase is the server's external URL.
func ApprovalLinksFor(c *secret.Cipher, apiBase string, ev *Event) {
	if !c.HasKey() || ev.Kind != EventApprovalNeeded || ev.JobID == 0 {
		return
	}
	exp := time.Now().Add(ApprovalLinkTTL)
	ev.Actions = map[string]ApprovalLinks{}
	for _, a := range ev.Approvers {
		a = strings.ToLower(strings.TrimSpace(a))
		if !strings.Contains(a, "@") {
			continue
		}
		mk := func(verdict string) string {
			return strings.TrimSuffix(apiBase, "/") + "/api/v1/approvals/link?t=" +
				url.QueryEscape(SignApprovalLink(c, ev.JobID, a, verdict, exp))
		}
		ev.Actions[a] = ApprovalLinks{Approve: mk("approved"), Reject: mk("rejected")}
	}
}
