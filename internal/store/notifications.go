package store

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// NotifySettings is where approval notifications go. Secret fields
// (SlackWebhookURL, SMTPPassword) are stored encrypted; reads for the UI
// blank them and report HasSlack / HasSMTPPassword instead.
type NotifySettings struct {
	SlackWebhookURL string `json:"slack_webhook_url,omitempty"`
	HasSlack        bool   `json:"has_slack"`
	EmailEnabled    bool   `json:"email_enabled"`
	SMTPHost        string `json:"smtp_host"`
	SMTPPort        int    `json:"smtp_port"`
	SMTPUsername    string `json:"smtp_username"`
	SMTPPassword    string `json:"smtp_password,omitempty"`
	HasSMTPPassword bool   `json:"has_smtp_password"`
	SMTPFrom        string `json:"smtp_from"`
	// ClearSlack removes a stored webhook URL (an empty URL keeps it).
	ClearSlack bool `json:"clear_slack,omitempty"`
}

const notifyKey = "notifications"

type storedNotify struct {
	SlackWebhookURL string `json:"slack_webhook_url"` // encrypted
	EmailEnabled    bool   `json:"email_enabled"`
	SMTPHost        string `json:"smtp_host"`
	SMTPPort        int    `json:"smtp_port"`
	SMTPUsername    string `json:"smtp_username"`
	SMTPPassword    string `json:"smtp_password"` // encrypted
	SMTPFrom        string `json:"smtp_from"`
}

func (s *Store) loadNotify(ctx context.Context) (storedNotify, error) {
	var st storedNotify
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT value FROM platform_settings WHERE key=$1`, notifyKey).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	return st, json.Unmarshal(raw, &st)
}

// GetNotifySettings returns the settings. With reveal, secrets are decrypted
// (for sending); otherwise they are blanked (for the admin UI).
func (s *Store) GetNotifySettings(ctx context.Context, reveal bool) (*NotifySettings, error) {
	st, err := s.loadNotify(ctx)
	if err != nil {
		return nil, err
	}
	out := &NotifySettings{
		HasSlack: st.SlackWebhookURL != "", EmailEnabled: st.EmailEnabled,
		SMTPHost: st.SMTPHost, SMTPPort: st.SMTPPort, SMTPUsername: st.SMTPUsername,
		HasSMTPPassword: st.SMTPPassword != "", SMTPFrom: st.SMTPFrom,
	}
	if reveal {
		if out.SlackWebhookURL, err = s.cipher.Decrypt(st.SlackWebhookURL); err != nil {
			return nil, err
		}
		if out.SMTPPassword, err = s.cipher.Decrypt(st.SMTPPassword); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// SaveNotifySettings stores the settings. An empty Slack URL or SMTP password
// keeps the stored one (so the UI can save without re-entering secrets);
// ClearSlack removes the Slack URL.
func (s *Store) SaveNotifySettings(ctx context.Context, in NotifySettings) error {
	st, err := s.loadNotify(ctx)
	if err != nil {
		return err
	}
	if in.ClearSlack {
		st.SlackWebhookURL = ""
	} else if in.SlackWebhookURL != "" {
		if st.SlackWebhookURL, err = s.cipher.Encrypt(in.SlackWebhookURL); err != nil {
			return err
		}
	}
	if in.SMTPPassword != "" {
		if st.SMTPPassword, err = s.cipher.Encrypt(in.SMTPPassword); err != nil {
			return err
		}
	}
	st.EmailEnabled, st.SMTPHost, st.SMTPPort = in.EmailEnabled, in.SMTPHost, in.SMTPPort
	st.SMTPUsername, st.SMTPFrom = in.SMTPUsername, in.SMTPFrom
	raw, _ := json.Marshal(st)
	_, err = s.pool.Exec(ctx,
		`INSERT INTO platform_settings (key, value) VALUES ($1,$2)
		 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`,
		notifyKey, raw)
	return err
}

// ApprovalNotice describes one job that just started waiting for approval.
type ApprovalNotice struct {
	JobID             int64
	JobName           string
	PipelineID        int64
	Repo              string
	Ref               string
	SHA               string
	Environment       string
	TriggeredBy       string
	CommitMessage     string
	RequiredApprovals int
	ApproverRoles     []string
	// RequiredGroup / RequiredGroupApprovals: "at least M from group X".
	RequiredGroup          string
	RequiredGroupApprovals int
	// Approvers are the people who may approve (emails when SSO is used).
	Approvers []string
}

// approvalNoticeWindow: only jobs blocked recently are notified, so turning
// notifications on (or upgrading) never floods anyone with old waits.
const approvalNoticeWindow = time.Hour

// ClaimApprovalNotices marks up to limit not-yet-notified approval waits as
// notified and returns them with their eligible approvers. The claim is a
// single UPDATE … SKIP LOCKED, so concurrent replicas never double-send.
// admins is the platform admin list, used for repos without members (where
// anyone may approve, so the admins are the people to tell).
func (s *Store) ClaimApprovalNotices(ctx context.Context, limit int, admins []string) ([]ApprovalNotice, error) {
	rows, err := s.pool.Query(ctx,
		`UPDATE jobs j SET approval_notified_at = now()
		 FROM (SELECT id FROM jobs
		       WHERE status = 'blocked' AND NOT manual AND approval_notified_at IS NULL
		         AND blocked_at > now() - $2::interval
		       ORDER BY id FOR UPDATE SKIP LOCKED LIMIT $1) c, pipelines p
		 WHERE j.id = c.id AND p.id = j.pipeline_id
		 RETURNING j.id, j.name, j.pipeline_id, p.repo, p.ref, p.sha,
		           COALESCE(j.environment, ''), p.triggered_by, p.commit_message`,
		limit, approvalNoticeWindow)
	if err != nil {
		return nil, err
	}
	var out []ApprovalNotice
	for rows.Next() {
		var n ApprovalNotice
		if err := rows.Scan(&n.JobID, &n.JobName, &n.PipelineID, &n.Repo, &n.Ref, &n.SHA,
			&n.Environment, &n.TriggeredBy, &n.CommitMessage); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if err := s.fillApprovers(ctx, &out[i], admins); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// fillApprovers resolves the environment's rule and the people who satisfy
// it. The pipeline's author is left out when self-approval is not allowed.
func (s *Store) fillApprovers(ctx context.Context, n *ApprovalNotice, admins []string) error {
	rule, err := s.resolveProtectedEnv(ctx, n.Repo, n.Environment)
	if err != nil {
		return err
	}
	selfOK := true
	n.RequiredApprovals = 1
	if rule != nil {
		n.RequiredApprovals, n.ApproverRoles, selfOK = rule.RequiredApprovals, rule.ApproverRoles, rule.AllowSelfApproval
		n.RequiredGroup, n.RequiredGroupApprovals = rule.RequiredGroup, rule.RequiredGroupApprovals
	}
	hasMembers, err := s.repoHasMembers(ctx, n.Repo)
	if err != nil {
		return err
	}
	set := map[string]bool{}
	if !hasMembers {
		for _, a := range admins {
			set[strings.ToLower(a)] = true
		}
	} else {
		rows, err := s.pool.Query(ctx,
			`SELECT lower(username) FROM repo_members WHERE repo = $1 AND role = ANY($2)`,
			n.Repo, n.ApproverRoles)
		if err != nil {
			return err
		}
		for rows.Next() {
			var u string
			if err := rows.Scan(&u); err != nil {
				rows.Close()
				return err
			}
			set[u] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		// People whose IdP groups grant an approver role on this repo.
		viaGroups, err := s.usersWithGroupRole(ctx, n.Repo, n.ApproverRoles)
		if err != nil {
			return err
		}
		for _, u := range viaGroups {
			set[u] = true
		}
	}
	group, err := s.groupMembers(ctx, s.pool, n.RequiredGroup)
	if err != nil {
		return err
	}
	for m := range group {
		set[m] = true
	}
	if !selfOK {
		delete(set, strings.ToLower(n.TriggeredBy))
	}
	n.Approvers = make([]string, 0, len(set))
	for u := range set {
		n.Approvers = append(n.Approvers, u)
	}
	sort.Strings(n.Approvers)
	return nil
}
