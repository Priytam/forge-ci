package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Merge rules, host identities, Forge reviews, gate results and the
// provider-write outbox (batch 7, phase B).

// FreezeWindow is a weekly window when merges wait, e.g. Fri 18:00 → Mon
// 09:00 in the rule's timezone. Days are mon..sun.
type FreezeWindow struct {
	Start string `json:"start"` // "fri 18:00"
	End   string `json:"end"`   // "mon 09:00"
}

// MergePolicy is one merge rule. It applies to changes of Repo ("" = every
// repo) whose target branch matches BranchPattern.
type MergePolicy struct {
	ID                      int64          `json:"id"`
	Repo                    string         `json:"repo"`
	BranchPattern           string         `json:"branch_pattern"`
	RequiredApprovals       int            `json:"required_approvals"`
	ApproverRoles           []string       `json:"approver_roles"`
	RequiredGroup           string         `json:"required_group"`
	RequiredGroupApprovals  int            `json:"required_group_approvals"`
	AllowSelfApproval       bool           `json:"allow_self_approval"`
	DismissOnPush           bool           `json:"dismiss_on_push"`
	BlockOnChangesRequested bool           `json:"block_on_changes_requested"`
	RequireCI               bool           `json:"require_ci"`
	RequiredJobs            []string       `json:"required_jobs"`
	RequiredLabels          []string       `json:"required_labels"`
	BlockedLabels           []string       `json:"blocked_labels"`
	TicketPattern           string         `json:"ticket_pattern"`
	FreezeWindows           []FreezeWindow `json:"freeze_windows"`
	Timezone                string         `json:"timezone"`
	UpdatedBy               string         `json:"updated_by"`
	UpdatedAt               time.Time      `json:"updated_at"`
}

var weekdays = map[string]int{"mon": 0, "tue": 1, "wed": 2, "thu": 3, "fri": 4, "sat": 5, "sun": 6}

// ParseWeekTime turns "fri 18:00" into minutes since Monday 00:00.
func ParseWeekTime(s string) (int, error) {
	f := strings.Fields(strings.ToLower(strings.TrimSpace(s)))
	if len(f) != 2 {
		return 0, fmt.Errorf("%q: want \"<day> HH:MM\", e.g. \"fri 18:00\"", s)
	}
	day, ok := weekdays[f[0][:min(3, len(f[0]))]]
	if !ok {
		return 0, fmt.Errorf("%q: day must be mon..sun", s)
	}
	t, err := time.Parse("15:04", f[1])
	if err != nil {
		return 0, fmt.Errorf("%q: time must be HH:MM", s)
	}
	return day*24*60 + t.Hour()*60 + t.Minute(), nil
}

// Validate normalises a rule and rejects one that could never be evaluated.
func (p *MergePolicy) Validate() error {
	p.Repo = strings.TrimSpace(p.Repo)
	p.BranchPattern = strings.TrimSpace(p.BranchPattern)
	if p.BranchPattern == "" {
		p.BranchPattern = "*"
	}
	if p.RequiredApprovals < 0 || p.RequiredGroupApprovals < 0 {
		return errors.New("approval counts cannot be negative")
	}
	if p.RequiredGroupApprovals > 0 && p.RequiredGroup == "" {
		return errors.New("required_group_approvals needs required_group")
	}
	p.RequiredGroup = strings.ToLower(strings.TrimSpace(p.RequiredGroup))
	if len(p.ApproverRoles) == 0 {
		p.ApproverRoles = []string{"admin", "owner", "developer"}
	}
	for _, r := range p.ApproverRoles {
		if r != "admin" && r != "owner" && r != "developer" {
			return fmt.Errorf("approver role %q must be admin, owner or developer", r)
		}
	}
	if p.TicketPattern != "" {
		if _, err := regexp.Compile(p.TicketPattern); err != nil {
			return fmt.Errorf("ticket_pattern: %v", err)
		}
	}
	if p.Timezone == "" {
		p.Timezone = "UTC"
	}
	if _, err := time.LoadLocation(p.Timezone); err != nil {
		return fmt.Errorf("timezone %q: %v", p.Timezone, err)
	}
	for _, w := range p.FreezeWindows {
		a, err := ParseWeekTime(w.Start)
		if err != nil {
			return err
		}
		b, err := ParseWeekTime(w.End)
		if err != nil {
			return err
		}
		if a == b {
			return errors.New("a freeze window cannot start and end at the same time")
		}
	}
	for _, l := range [][]string{p.RequiredJobs, p.RequiredLabels, p.BlockedLabels} {
		for _, v := range l {
			if strings.TrimSpace(v) == "" {
				return errors.New("job and label names cannot be empty")
			}
		}
	}
	nonNil := func(s []string) []string {
		if s == nil {
			return []string{}
		}
		return s
	}
	p.RequiredJobs, p.RequiredLabels, p.BlockedLabels = nonNil(p.RequiredJobs), nonNil(p.RequiredLabels), nonNil(p.BlockedLabels)
	if p.FreezeWindows == nil {
		p.FreezeWindows = []FreezeWindow{}
	}
	return nil
}

const mergePolicyCols = `id, repo, branch_pattern, required_approvals, approver_roles, required_group,
	required_group_approvals, allow_self_approval, dismiss_on_push, block_on_changes_requested,
	require_ci, required_jobs, required_labels, blocked_labels, ticket_pattern, freeze_windows,
	timezone, updated_by, updated_at`

func scanMergePolicy(row pgx.Row) (*MergePolicy, error) {
	var p MergePolicy
	var windows []byte
	if err := row.Scan(&p.ID, &p.Repo, &p.BranchPattern, &p.RequiredApprovals, &p.ApproverRoles,
		&p.RequiredGroup, &p.RequiredGroupApprovals, &p.AllowSelfApproval, &p.DismissOnPush,
		&p.BlockOnChangesRequested, &p.RequireCI, &p.RequiredJobs, &p.RequiredLabels, &p.BlockedLabels,
		&p.TicketPattern, &windows, &p.Timezone, &p.UpdatedBy, &p.UpdatedAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(windows, &p.FreezeWindows); err != nil {
		return nil, err
	}
	return &p, nil
}

// ListMergePolicies returns every rule (repo ” first, then by repo/pattern).
func (s *Store) ListMergePolicies(ctx context.Context) ([]MergePolicy, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+mergePolicyCols+` FROM merge_policies ORDER BY repo, branch_pattern`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MergePolicy{}
	for rows.Next() {
		p, err := scanMergePolicy(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// UpsertMergePolicy saves a rule keyed by (repo, branch_pattern) and marks
// every open change's gate for re-evaluation.
func (s *Store) UpsertMergePolicy(ctx context.Context, p MergePolicy) (*MergePolicy, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	windows, _ := json.Marshal(p.FreezeWindows)
	out, err := scanMergePolicy(s.pool.QueryRow(ctx,
		`INSERT INTO merge_policies (repo, branch_pattern, required_approvals, approver_roles, required_group,
		     required_group_approvals, allow_self_approval, dismiss_on_push, block_on_changes_requested,
		     require_ci, required_jobs, required_labels, blocked_labels, ticket_pattern, freeze_windows,
		     timezone, updated_by)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
		 ON CONFLICT (repo, branch_pattern) DO UPDATE SET
		     required_approvals = EXCLUDED.required_approvals, approver_roles = EXCLUDED.approver_roles,
		     required_group = EXCLUDED.required_group, required_group_approvals = EXCLUDED.required_group_approvals,
		     allow_self_approval = EXCLUDED.allow_self_approval, dismiss_on_push = EXCLUDED.dismiss_on_push,
		     block_on_changes_requested = EXCLUDED.block_on_changes_requested, require_ci = EXCLUDED.require_ci,
		     required_jobs = EXCLUDED.required_jobs, required_labels = EXCLUDED.required_labels,
		     blocked_labels = EXCLUDED.blocked_labels, ticket_pattern = EXCLUDED.ticket_pattern,
		     freeze_windows = EXCLUDED.freeze_windows, timezone = EXCLUDED.timezone,
		     updated_by = EXCLUDED.updated_by, updated_at = now()
		 RETURNING `+mergePolicyCols,
		p.Repo, p.BranchPattern, p.RequiredApprovals, p.ApproverRoles, p.RequiredGroup,
		p.RequiredGroupApprovals, p.AllowSelfApproval, p.DismissOnPush, p.BlockOnChangesRequested,
		p.RequireCI, p.RequiredJobs, p.RequiredLabels, p.BlockedLabels, p.TicketPattern, windows,
		p.Timezone, p.UpdatedBy))
	if err != nil {
		return nil, err
	}
	return out, s.MarkAllGatesDirty(ctx)
}

// DeleteMergePolicy removes a rule; the changes it governed are re-evaluated.
func (s *Store) DeleteMergePolicy(ctx context.Context, id int64) (*MergePolicy, error) {
	p, err := scanMergePolicy(s.pool.QueryRow(ctx,
		`DELETE FROM merge_policies WHERE id = $1 RETURNING `+mergePolicyCols, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return p, s.MarkAllGatesDirty(ctx)
}

// MatchMergePolicy picks the rule for a change: a repo's own rules beat
// global ones, an exact branch beats a pattern, a longer pattern beats a
// shorter one. nil when none matches.
func MatchMergePolicy(policies []MergePolicy, repo, target string) *MergePolicy {
	var best *MergePolicy
	score := func(p *MergePolicy) int {
		n := len(p.BranchPattern)
		if !strings.Contains(p.BranchPattern, "*") {
			n += 1000
		}
		if p.Repo != "" {
			n += 100000
		}
		return n
	}
	for i := range policies {
		p := &policies[i]
		if (p.Repo != "" && p.Repo != repo) || !globMatch(p.BranchPattern, target) {
			continue
		}
		if best == nil || score(p) > score(best) {
			best = p
		}
	}
	return best
}

// MarkAllGatesDirty asks for every open change's gate to be re-evaluated
// (a rule or an identity changed).
func (s *Store) MarkAllGatesDirty(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `UPDATE changes SET gate_dirty_at = now() WHERE state = 'open'`)
	return err
}

// ---- host identities ----

// HostIdentity maps a login on a host to a Forge user (email).
type HostIdentity struct {
	Provider string    `json:"provider"`
	Login    string    `json:"login"`
	Email    string    `json:"email"`
	Created  time.Time `json:"created_at"`
}

func (s *Store) ListHostIdentities(ctx context.Context) ([]HostIdentity, error) {
	rows, err := s.pool.Query(ctx, `SELECT provider, login, email, created_at FROM host_identities ORDER BY provider, login`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []HostIdentity{}
	for rows.Next() {
		var h HostIdentity
		if err := rows.Scan(&h.Provider, &h.Login, &h.Email, &h.Created); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// PutHostIdentity links (or relinks) a host login to an email.
func (s *Store) PutHostIdentity(ctx context.Context, h HostIdentity) error {
	h.Login = strings.ToLower(strings.TrimSpace(h.Login))
	h.Email = strings.ToLower(strings.TrimSpace(h.Email))
	if h.Provider == "" || h.Login == "" || !strings.Contains(h.Email, "@") {
		return errors.New("provider, login and an email are required")
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO host_identities (provider, login, email) VALUES ($1,$2,$3)
		 ON CONFLICT (provider, login) DO UPDATE SET email = EXCLUDED.email`, h.Provider, h.Login, h.Email); err != nil {
		return err
	}
	return s.MarkAllGatesDirty(ctx)
}

func (s *Store) DeleteHostIdentity(ctx context.Context, provider, login string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM host_identities WHERE provider = $1 AND login = $2`,
		provider, strings.ToLower(login))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return s.MarkAllGatesDirty(ctx)
}

// hostIdentityMap is provider's login → email.
func (s *Store) hostIdentityMap(ctx context.Context, provider string) (map[string]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT login, email FROM host_identities WHERE provider = $1`, provider)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var l, e string
		if err := rows.Scan(&l, &e); err != nil {
			return nil, err
		}
		out[l] = e
	}
	return out, rows.Err()
}

// ---- Forge reviews ----

// ErrChangeClosed: the change is no longer open.
var ErrChangeClosed = errors.New("change is not open")

// ReviewTargetChange is what a review or merge request needs about a change.
type ReviewTargetChange struct {
	ID           int64
	Repo         string
	Provider     string
	Number       string
	Author       string
	TargetBranch string
	State        string
	Draft        bool
	HeadSHA      string
}

// ChangeForAction loads a change for a write (review, merge).
func (s *Store) ChangeForAction(ctx context.Context, id int64) (*ReviewTargetChange, error) {
	var c ReviewTargetChange
	err := s.pool.QueryRow(ctx,
		`SELECT id, repo, provider, number, author, target_branch, state, draft, head_sha
		 FROM changes WHERE id = $1 AND synced_at IS NOT NULL`, id).
		Scan(&c.ID, &c.Repo, &c.Provider, &c.Number, &c.Author, &c.TargetBranch, &c.State, &c.Draft, &c.HeadSHA)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &c, err
}

// SetForgeReview records reviewer's verdict given in Forge (one live verdict
// per person; "dismissed" withdraws it) on the current head, and marks the
// gate for re-evaluation.
func (s *Store) SetForgeReview(ctx context.Context, changeID int64, reviewer, verdict, body, headSHA string) error {
	reviewer = strings.ToLower(reviewer)
	_, err := s.pool.Exec(ctx,
		`INSERT INTO change_reviews (change_id, source, external_id, reviewer, verdict, body, commit_sha, submitted_at)
		 VALUES ($1, 'forge', $2, $2, $3, $4, $5, now())
		 ON CONFLICT (change_id, source, external_id) DO UPDATE SET
		     verdict = EXCLUDED.verdict, body = EXCLUDED.body, commit_sha = EXCLUDED.commit_sha, submitted_at = now()`,
		changeID, reviewer, verdict, body, headSHA)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `UPDATE changes SET gate_dirty_at = now() WHERE id = $1`, changeID)
	if err == nil {
		_, err = s.pool.Exec(ctx, `SELECT pg_notify($1, $2)`, changesChannel, strconv.FormatInt(changeID, 10))
	}
	return err
}

// ApproverEligibility says which of emails may approve on repo under a rule:
// a repo role in roles, or membership of the rule's group. bootstrap is true
// for a repo with no members, where anyone may (docs/rbac-approvals.md).
func (s *Store) ApproverEligibility(ctx context.Context, repo string, emails, roles []string, group string) (eligible, groupMembers map[string]bool, bootstrap bool, err error) {
	eligible = map[string]bool{}
	groupMembers, err = s.groupMembers(ctx, s.pool, group)
	if err != nil {
		return nil, nil, false, err
	}
	has, err := s.repoHasMembers(ctx, repo)
	if err != nil {
		return nil, nil, false, err
	}
	for _, e := range emails {
		e = strings.ToLower(e)
		if !has || groupMembers[e] {
			eligible[e] = true
			continue
		}
		role, err := s.memberRole(ctx, repo, e)
		if err != nil {
			return nil, nil, false, err
		}
		for _, r := range roles {
			if role == r {
				eligible[e] = true
			}
		}
	}
	return eligible, groupMembers, !has, nil
}

// ---- gate ----

// GateRule is one rule's outcome.
type GateRule struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	State  string `json:"state"` // pass | pending | fail
	Detail string `json:"detail"`
}

// GateResult is the gate's verdict for a change's head.
type GateResult struct {
	ChangeID       int64      `json:"change_id"`
	HeadSHA        string     `json:"head_sha"`
	PolicyID       *int64     `json:"policy_id,omitempty"`
	State          string     `json:"state"`
	Summary        string     `json:"summary"`
	Rules          []GateRule `json:"rules"`
	EvaluatedAt    time.Time  `json:"evaluated_at"`
	PublishedState string     `json:"published_state"`
	PublishedAt    *time.Time `json:"published_at,omitempty"`
}

// HeadPipeline is the newest Forge pipeline on a commit.
type HeadPipeline struct {
	ID     int64
	Status string
	Jobs   map[string]string // name → status (allow_failure failures read as success)
}

// GateInputs is everything one gate evaluation reads.
type GateInputs struct {
	Change     ReviewTargetChange
	Title      string
	Body       string
	Source     string
	Labels     []string
	Reviews    []ChangeReview
	Pipeline   *HeadPipeline
	Identities map[string]string // host login → email
}

// ClaimDirtyGates returns open changes whose gate must be re-evaluated (an
// input moved, or a freeze window boundary passed), up to limit, and the
// claim time used to clear them.
func (s *Store) ClaimDirtyGates(ctx context.Context, limit int) ([]int64, time.Time, error) {
	var now time.Time
	if err := s.pool.QueryRow(ctx, `SELECT now()`).Scan(&now); err != nil {
		return nil, now, err
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id FROM changes WHERE state = 'open' AND synced_at IS NOT NULL
		   AND (gate_dirty_at IS NOT NULL OR gate_recheck_at <= now())
		 ORDER BY COALESCE(gate_dirty_at, gate_recheck_at) LIMIT $1`, limit)
	if err != nil {
		return nil, now, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, now, err
		}
		ids = append(ids, id)
	}
	return ids, now, rows.Err()
}

// LoadGateInputs reads one change's gate inputs (Postgres only).
func (s *Store) LoadGateInputs(ctx context.Context, id int64) (*GateInputs, error) {
	in := &GateInputs{}
	c := &in.Change
	err := s.pool.QueryRow(ctx,
		`SELECT id, repo, provider, number, author, target_branch, state, draft, head_sha,
		        title, body, source_branch, labels
		 FROM changes WHERE id = $1`, id).
		Scan(&c.ID, &c.Repo, &c.Provider, &c.Number, &c.Author, &c.TargetBranch, &c.State, &c.Draft, &c.HeadSHA,
			&in.Title, &in.Body, &in.Source, &in.Labels)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, source, reviewer, verdict, body, commit_sha, submitted_at FROM change_reviews
		 WHERE change_id = $1 ORDER BY submitted_at, id`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var r ChangeReview
		if err := rows.Scan(&r.ID, &r.Source, &r.Reviewer, &r.Verdict, &r.Body, &r.CommitSHA, &r.SubmittedAt); err != nil {
			rows.Close()
			return nil, err
		}
		in.Reviews = append(in.Reviews, r)
	}
	rows.Close()
	var pid int64
	var status string
	err = s.pool.QueryRow(ctx,
		`SELECT id, status FROM pipelines WHERE repo = $1 AND sha = $2 AND $2 <> '' ORDER BY id DESC LIMIT 1`,
		c.Repo, c.HeadSHA).Scan(&pid, &status)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return nil, err
	default:
		hp := &HeadPipeline{ID: pid, Status: status, Jobs: map[string]string{}}
		jrows, err := s.pool.Query(ctx,
			`SELECT name, CASE WHEN status = 'failed' AND allow_failure THEN 'success' ELSE status END
			 FROM jobs WHERE pipeline_id = $1`, pid)
		if err != nil {
			return nil, err
		}
		for jrows.Next() {
			var n, st string
			if err := jrows.Scan(&n, &st); err != nil {
				jrows.Close()
				return nil, err
			}
			hp.Jobs[n] = st
		}
		jrows.Close()
		in.Pipeline = hp
	}
	if in.Identities, err = s.hostIdentityMap(ctx, c.Provider); err != nil {
		return nil, err
	}
	return in, nil
}

func rulesFingerprint(state, summary string, rules []GateRule) string {
	b, _ := json.Marshal(rules)
	return state + "|" + summary + "|" + string(b)
}

// SaveGateResult stores a change's verdict for its head when it differs from
// the stored one, clears the dirty mark (unless something newer arrived since
// claimedAt) and sets the next time-based recheck. changed reports whether
// the verdict moved.
func (s *Store) SaveGateResult(ctx context.Context, g GateResult, claimedAt time.Time, recheck *time.Time) (changed bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var oldState, oldSummary string
	var oldRules []byte
	err = tx.QueryRow(ctx,
		`SELECT state, summary, rules FROM gate_results WHERE change_id = $1 AND head_sha = $2 FOR UPDATE`,
		g.ChangeID, g.HeadSHA).Scan(&oldState, &oldSummary, &oldRules)
	var prev []GateRule
	if err == nil {
		_ = json.Unmarshal(oldRules, &prev)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	if g.Rules == nil {
		g.Rules = []GateRule{}
	}
	changed = errors.Is(err, pgx.ErrNoRows) ||
		rulesFingerprint(oldState, oldSummary, prev) != rulesFingerprint(g.State, g.Summary, g.Rules)
	if changed {
		rules, _ := json.Marshal(g.Rules)
		if _, err := tx.Exec(ctx,
			`INSERT INTO gate_results (change_id, head_sha, policy_id, state, summary, rules, evaluated_at)
			 VALUES ($1,$2,$3,$4,$5,$6, now())
			 ON CONFLICT (change_id, head_sha) DO UPDATE SET policy_id = EXCLUDED.policy_id,
			     state = EXCLUDED.state, summary = EXCLUDED.summary, rules = EXCLUDED.rules, evaluated_at = now()`,
			g.ChangeID, g.HeadSHA, g.PolicyID, g.State, g.Summary, rules); err != nil {
			return false, err
		}
		if _, err := tx.Exec(ctx, `SELECT pg_notify($1, $2)`, changesChannel, strconv.FormatInt(g.ChangeID, 10)); err != nil {
			return false, err
		}
	}
	if _, err := tx.Exec(ctx,
		`UPDATE changes SET gate_recheck_at = $3,
		     gate_dirty_at = CASE WHEN gate_dirty_at > $2 THEN gate_dirty_at END
		 WHERE id = $1`, g.ChangeID, claimedAt, recheck); err != nil {
		return false, err
	}
	return changed, tx.Commit(ctx)
}

// ClearGate removes a change's gate (no rule matches any more) and clears
// the dirty mark. Returns whether a stored result was removed.
func (s *Store) ClearGate(ctx context.Context, changeID int64, claimedAt time.Time) (bool, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM gate_results WHERE change_id = $1`, changeID)
	if err != nil {
		return false, err
	}
	_, err = s.pool.Exec(ctx,
		`UPDATE changes SET gate_recheck_at = NULL,
		     gate_dirty_at = CASE WHEN gate_dirty_at > $2 THEN gate_dirty_at END WHERE id = $1`, changeID, claimedAt)
	return tag.RowsAffected() > 0, err
}

// GetGate returns the gate for a change's current head (nil: no rule applies).
func (s *Store) GetGate(ctx context.Context, changeID int64) (*GateResult, error) {
	g := &GateResult{}
	var rules []byte
	err := s.pool.QueryRow(ctx,
		`SELECT g.change_id, g.head_sha, g.policy_id, g.state, g.summary, g.rules, g.evaluated_at,
		        g.published_state, g.published_at
		 FROM gate_results g JOIN changes c ON c.id = g.change_id AND c.head_sha = g.head_sha
		 WHERE g.change_id = $1`, changeID).
		Scan(&g.ChangeID, &g.HeadSHA, &g.PolicyID, &g.State, &g.Summary, &rules, &g.EvaluatedAt,
			&g.PublishedState, &g.PublishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return g, json.Unmarshal(rules, &g.Rules)
}

// MarkGatePublished records what the host's forge/merge-gate now shows.
func (s *Store) MarkGatePublished(ctx context.Context, changeID int64, headSHA, state string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE gate_results SET published_state = $3, published_at = now() WHERE change_id = $1 AND head_sha = $2`,
		changeID, headSHA, state)
	if err == nil {
		_, err = s.pool.Exec(ctx, `SELECT pg_notify($1, $2)`, changesChannel, strconv.FormatInt(changeID, 10))
	}
	return err
}

// ---- outbox ----

// OutboxItem is one queued provider write.
type OutboxItem struct {
	ID            int64           `json:"id"`
	IdemKey       string          `json:"-"`
	Kind          string          `json:"kind"`
	Repo          string          `json:"repo"`
	ChangeID      *int64          `json:"change_id,omitempty"`
	Class         string          `json:"class"`
	Payload       json.RawMessage `json:"payload"`
	State         string          `json:"state"`
	Attempts      int             `json:"attempts"`
	NextAttemptAt time.Time       `json:"next_attempt_at"`
	LastError     string          `json:"last_error,omitempty"`
	Result        json.RawMessage `json:"result,omitempty"`
	RequestedBy   string          `json:"requested_by,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
	DoneAt        *time.Time      `json:"done_at,omitempty"`
}

const outboxCols = `id, idem_key, kind, repo, change_id, class, payload, state, attempts, next_attempt_at,
	last_error, result, requested_by, created_at, done_at`

func scanOutbox(row pgx.Row) (*OutboxItem, error) {
	var o OutboxItem
	err := row.Scan(&o.ID, &o.IdemKey, &o.Kind, &o.Repo, &o.ChangeID, &o.Class, &o.Payload, &o.State,
		&o.Attempts, &o.NextAttemptAt, &o.LastError, &o.Result, &o.RequestedBy, &o.CreatedAt, &o.DoneAt)
	return &o, err
}

// EnqueueOutbox saves a provider write. The idempotency key makes a repeat
// (double click, retry, a second replica) return the existing row; a failed
// row with the same key is re-armed. supersedeKind, when set, retires unsent
// rows of that kind for the same change (only the newest gate is sent).
func (s *Store) EnqueueOutbox(ctx context.Context, o OutboxItem, supersedeKind string) (*OutboxItem, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if supersedeKind != "" && o.ChangeID != nil {
		if _, err := tx.Exec(ctx,
			`UPDATE provider_outbox SET state = 'superseded', done_at = now()
			 WHERE change_id = $1 AND kind = $2 AND state = 'pending' AND idem_key <> $3`,
			*o.ChangeID, supersedeKind, o.IdemKey); err != nil {
			return nil, err
		}
	}
	if o.Payload == nil {
		o.Payload = json.RawMessage(`{}`)
	}
	out, err := scanOutbox(tx.QueryRow(ctx,
		`INSERT INTO provider_outbox (idem_key, kind, repo, change_id, class, payload, requested_by)
		 VALUES ($1,$2,$3,$4,$5,$6,$7)
		 ON CONFLICT (idem_key) DO UPDATE SET
		     state = CASE WHEN provider_outbox.state IN ('failed','superseded') THEN 'pending' ELSE provider_outbox.state END,
		     attempts = CASE WHEN provider_outbox.state IN ('failed','superseded') THEN 0 ELSE provider_outbox.attempts END,
		     next_attempt_at = CASE WHEN provider_outbox.state IN ('failed','superseded') THEN now() ELSE provider_outbox.next_attempt_at END,
		     last_error = CASE WHEN provider_outbox.state IN ('failed','superseded') THEN '' ELSE provider_outbox.last_error END,
		     requested_by = CASE WHEN provider_outbox.state IN ('failed','superseded') THEN EXCLUDED.requested_by ELSE provider_outbox.requested_by END,
		     done_at = CASE WHEN provider_outbox.state IN ('failed','superseded') THEN NULL ELSE provider_outbox.done_at END
		 RETURNING `+outboxCols,
		o.IdemKey, o.Kind, o.Repo, o.ChangeID, o.Class, o.Payload, o.RequestedBy))
	if err != nil {
		return nil, err
	}
	if o.ChangeID != nil {
		if _, err := tx.Exec(ctx, `SELECT pg_notify($1, $2)`, changesChannel, strconv.FormatInt(*o.ChangeID, 10)); err != nil {
			return nil, err
		}
	}
	return out, tx.Commit(ctx)
}

// ClaimOutbox leases up to limit due writes, user actions first.
func (s *Store) ClaimOutbox(ctx context.Context, limit int, lease time.Duration) ([]OutboxItem, error) {
	rows, err := s.pool.Query(ctx,
		`UPDATE provider_outbox SET lease_until = now() + $2 * interval '1 millisecond'
		 WHERE id IN (
		     SELECT id FROM provider_outbox
		     WHERE state = 'pending' AND next_attempt_at <= now()
		       AND (lease_until IS NULL OR lease_until < now())
		     ORDER BY CASE class WHEN 'user' THEN 0 WHEN 'webhook' THEN 1 ELSE 2 END, next_attempt_at
		     LIMIT $1 FOR UPDATE SKIP LOCKED)
		 RETURNING `+outboxCols, limit, lease.Milliseconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OutboxItem
	for rows.Next() {
		o, err := scanOutbox(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *o)
	}
	return out, rows.Err()
}

// FinishOutbox records a write's outcome: done (result), failed (permanent,
// reason kept), or retry at retryAt. countAttempt=false defers without using
// up a try (e.g. a merge waiting for its gate to reach the host).
func (s *Store) FinishOutbox(ctx context.Context, o OutboxItem, state string, result any, failure error, retryAt time.Time, countAttempt bool) error {
	res, _ := json.Marshal(result)
	if result == nil {
		res = []byte(`{}`)
	}
	msg := ""
	if failure != nil {
		msg = truncate(failure.Error(), 300)
	}
	inc := 0
	if countAttempt {
		inc = 1
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE provider_outbox SET state = $2, result = $3, last_error = $4, attempts = attempts + $5,
		     next_attempt_at = COALESCE($6, next_attempt_at), lease_until = NULL,
		     done_at = CASE WHEN $2 IN ('done','failed') THEN now() END
		 WHERE id = $1 AND state = 'pending'`,
		o.ID, state, res, msg, inc, nullTime(retryAt))
	if err == nil && o.ChangeID != nil {
		_, err = s.pool.Exec(ctx, `SELECT pg_notify($1, $2)`, changesChannel, strconv.FormatInt(*o.ChangeID, 10))
	}
	return err
}

// ChangeActions returns a change's recent provider writes, newest first.
func (s *Store) ChangeActions(ctx context.Context, changeID int64, limit int) ([]OutboxItem, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+outboxCols+` FROM provider_outbox WHERE change_id = $1 ORDER BY id DESC LIMIT $2`, changeID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OutboxItem{}
	for rows.Next() {
		o, err := scanOutbox(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *o)
	}
	return out, rows.Err()
}

// OutboxBacklog is how many writes are pending and the oldest one's age.
func (s *Store) OutboxBacklog(ctx context.Context) (n int, oldest time.Duration, err error) {
	var secs float64
	err = s.pool.QueryRow(ctx,
		`SELECT count(*), COALESCE(EXTRACT(EPOCH FROM now() - min(created_at)), 0)
		 FROM provider_outbox WHERE state = 'pending'`).Scan(&n, &secs)
	return n, time.Duration(secs * float64(time.Second)), err
}

// GateStats counts gate verdicts written in a window and how many changes
// are blocked now, by rule.
type GateStats struct {
	Evaluations int64          `json:"evaluations"`
	BlockedBy   map[string]int `json:"blocked_by"`
}

func (s *Store) GateStatsSince(ctx context.Context, since time.Time) (GateStats, error) {
	st := GateStats{BlockedBy: map[string]int{}}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM gate_results WHERE evaluated_at >= $1`, since).
		Scan(&st.Evaluations); err != nil {
		return st, err
	}
	rows, err := s.pool.Query(ctx,
		`SELECT r->>'id', count(*) FROM gate_results g JOIN changes c ON c.id = g.change_id AND c.head_sha = g.head_sha
		 CROSS JOIN LATERAL jsonb_array_elements(g.rules) r
		 WHERE c.state = 'open' AND r->>'state' <> 'pass' GROUP BY 1`)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return st, err
		}
		st.BlockedBy[id] = n
	}
	return st, rows.Err()
}

// HasGate reports whether a change has ever had a gate (so removing its rule
// must publish a passing gate instead of leaving the host blocked).
func (s *Store) HasGate(ctx context.Context, changeID int64) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM gate_results WHERE change_id = $1)`, changeID).Scan(&ok)
	return ok, err
}

// HostIdentityEmail returns the Forge user a host login belongs to ("" when
// it is not linked).
func (s *Store) HostIdentityEmail(ctx context.Context, provider, login string) (string, error) {
	var email string
	err := s.pool.QueryRow(ctx, `SELECT email FROM host_identities WHERE provider = $1 AND login = $2`,
		provider, strings.ToLower(login)).Scan(&email)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return email, err
}
