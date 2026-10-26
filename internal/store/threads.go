package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/priytamjeepandey/forge-ci/internal/vcs"
)

// Threads and comments (batch 7, phase D).

// ThreadComment is one comment as pages show it.
type ThreadComment struct {
	ID          int64     `json:"id"`
	Author      string    `json:"author"`
	AuthorEmail string    `json:"author_email,omitempty"`
	Body        string    `json:"body"`
	Source      string    `json:"source"` // host | forge
	State       string    `json:"state"`  // sent | pending | failed | local (Forge-only host)
	Error       string    `json:"error,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// ChangeThread is one thread with its comments.
type ChangeThread struct {
	ID             int64           `json:"id"`
	ExternalID     string          `json:"external_id,omitempty"`
	Path           string          `json:"path"`
	Side           string          `json:"side"`
	Line           int             `json:"line"`
	StartLine      int             `json:"start_line,omitempty"`
	OriginalLine   int             `json:"original_line,omitempty"`
	CommitSHA      string          `json:"commit_sha,omitempty"`
	OriginalCommit string          `json:"original_commit,omitempty"`
	Resolved       bool            `json:"resolved"`
	Outdated       bool            `json:"outdated"`
	ResolvePending bool            `json:"resolve_pending,omitempty"`
	Source         string          `json:"source"`
	CreatedBy      string          `json:"created_by,omitempty"`
	Comments       []ThreadComment `json:"comments"`
}

// ChangeThreads returns a change's threads, oldest first, conversation first.
func (s *Store) ChangeThreads(ctx context.Context, changeID int64) ([]ChangeThread, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, external_id, path, side, line, start_line, original_line, commit_sha, original_commit,
		        resolved, outdated, resolve_pending, source, created_by
		 FROM change_threads WHERE change_id = $1 ORDER BY (path <> ''), created_at, id`, changeID)
	if err != nil {
		return nil, err
	}
	out := []ChangeThread{}
	index := map[int64]int{}
	for rows.Next() {
		var t ChangeThread
		if err := rows.Scan(&t.ID, &t.ExternalID, &t.Path, &t.Side, &t.Line, &t.StartLine, &t.OriginalLine,
			&t.CommitSHA, &t.OriginalCommit, &t.Resolved, &t.Outdated, &t.ResolvePending, &t.Source, &t.CreatedBy); err != nil {
			rows.Close()
			return nil, err
		}
		t.Comments = []ThreadComment{}
		index[t.ID] = len(out)
		out = append(out, t)
	}
	rows.Close()
	crows, err := s.pool.Query(ctx,
		`SELECT c.thread_id, c.id, c.author, c.author_email, c.body, c.source, c.state, c.error, c.created_at
		 FROM change_comments c JOIN change_threads t ON t.id = c.thread_id
		 WHERE t.change_id = $1 ORDER BY c.created_at, c.id`, changeID)
	if err != nil {
		return nil, err
	}
	defer crows.Close()
	for crows.Next() {
		var tid int64
		var c ThreadComment
		if err := crows.Scan(&tid, &c.ID, &c.Author, &c.AuthorEmail, &c.Body, &c.Source, &c.State, &c.Error, &c.CreatedAt); err != nil {
			return nil, err
		}
		if i, ok := index[tid]; ok {
			out[i].Comments = append(out[i].Comments, c)
		}
	}
	return out, crows.Err()
}

func nonZero(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now()
	}
	return t
}

// applyThreadsTx mirrors the host's threads into Forge. A host comment that
// carries the marker of a Forge comment is that comment coming back: it is
// linked (external id, state sent), never stored twice. Host comments and
// threads that disappeared on the host are removed; Forge comments not yet
// on the host are left alone.
func applyThreadsTx(ctx context.Context, tx pgx.Tx, changeID int64, threads []vcs.Thread) error {
	type forgeRef struct{ id, thread int64 }
	byExt := map[string]int64{}
	byUID := map[string]forgeRef{}
	rows, err := tx.Query(ctx,
		`SELECT t.id, t.external_id, COALESCE(c.id, 0), COALESCE(c.forge_uid, '')
		 FROM change_threads t LEFT JOIN change_comments c ON c.thread_id = t.id AND c.forge_uid <> ''
		 WHERE t.change_id = $1`, changeID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var tid, cid int64
		var ext, uid string
		if err := rows.Scan(&tid, &ext, &cid, &uid); err != nil {
			rows.Close()
			return err
		}
		if ext != "" {
			byExt[ext] = tid
		}
		if uid != "" {
			byUID[uid] = forgeRef{id: cid, thread: tid}
		}
	}
	rows.Close()
	keepThreads := []string{vcs.ConversationThread}
	for _, ht := range threads {
		keepThreads = append(keepThreads, ht.ExternalID)
		tid := byExt[ht.ExternalID]
		if tid == 0 {
			for _, c := range ht.Comments {
				if _, uid := vcs.Unmark(c.Body); uid != "" {
					if ref, ok := byUID[uid]; ok {
						tid = ref.thread // a thread Forge started, back from the host
						break
					}
				}
			}
		}
		root := ""
		if len(ht.Comments) > 0 {
			root = ht.Comments[0].ExternalID
		}
		if tid == 0 && len(ht.Comments) == 0 {
			continue // nothing said yet (an empty conversation): no row
		}
		if tid == 0 {
			if err := tx.QueryRow(ctx,
				`INSERT INTO change_threads (change_id, external_id, root_comment_ext, path, side, line, start_line,
				     original_line, commit_sha, original_commit, resolved, outdated, source)
				 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,'host')
				 ON CONFLICT (change_id, external_id) WHERE external_id <> '' DO UPDATE SET updated_at = now()
				 RETURNING id`,
				changeID, ht.ExternalID, root, ht.Path, sideOf(ht.Side), ht.Line, ht.StartLine, ht.OriginalLine,
				ht.CommitSHA, ht.OriginalCommit, ht.Resolved, ht.Outdated).Scan(&tid); err != nil {
				return err
			}
		} else if _, err := tx.Exec(ctx,
			`UPDATE change_threads SET external_id = $2, root_comment_ext = CASE WHEN $3 = '' THEN root_comment_ext ELSE $3 END,
			     line = $4, start_line = $5, original_line = $6,
			     commit_sha = CASE WHEN $7 = '' THEN commit_sha ELSE $7 END,
			     original_commit = CASE WHEN $8 = '' THEN original_commit ELSE $8 END,
			     resolved = CASE WHEN resolve_pending THEN resolved ELSE $9 END, outdated = $10, updated_at = now()
			 WHERE id = $1`,
			tid, ht.ExternalID, root, ht.Line, ht.StartLine, ht.OriginalLine, ht.CommitSHA, ht.OriginalCommit,
			ht.Resolved, ht.Outdated); err != nil {
			return err
		}
		keep := []string{}
		for _, c := range ht.Comments {
			keep = append(keep, c.ExternalID)
			body, uid := vcs.Unmark(c.Body)
			if ref, ok := byUID[uid]; ok && uid != "" {
				if _, err := tx.Exec(ctx,
					`UPDATE change_comments SET external_id = $2, thread_id = $3, state = 'sent', error = '', updated_at = now()
					 WHERE id = $1`, ref.id, c.ExternalID, tid); err != nil {
					return err
				}
				continue
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO change_comments (thread_id, external_id, author, body, source, created_at, updated_at)
				 VALUES ($1,$2,$3,$4,'host',$5,$6)
				 ON CONFLICT (thread_id, external_id) WHERE external_id <> '' DO UPDATE SET
				     author = EXCLUDED.author, body = EXCLUDED.body, updated_at = EXCLUDED.updated_at`,
				tid, c.ExternalID, c.Author, body, nonZero(c.CreatedAt), nonZero(c.UpdatedAt)); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx,
			`DELETE FROM change_comments WHERE thread_id = $1 AND source = 'host' AND NOT (external_id = ANY($2))`,
			tid, keep); err != nil {
			return err
		}
	}
	// Threads deleted on the host (all their comments gone), unless Forge
	// still has something of its own in them.
	_, err = tx.Exec(ctx,
		`DELETE FROM change_threads t WHERE t.change_id = $1 AND t.source = 'host' AND t.external_id <> ''
		   AND NOT (t.external_id = ANY($2))
		   AND NOT EXISTS (SELECT 1 FROM change_comments c WHERE c.thread_id = t.id AND c.source = 'forge')`,
		changeID, keepThreads)
	return err
}

func sideOf(s string) string {
	if s == "old" {
		return "old"
	}
	return "new"
}

func newUID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// NewComment is a comment someone writes in Forge.
type NewComment struct {
	Author      string // display name or email
	AuthorEmail string
	Body        string
	// Sync: the host has threads, so the comment is sent there (state
	// pending until it is); otherwise it lives in Forge only (state local).
	Sync bool
}

// NewThreadAnchor places a new thread ("" path: the conversation).
type NewThreadAnchor struct {
	Path        string
	Side        string
	Line        int
	StartLine   int
	CommitSHA   string
	AnchorHead  string
	Fingerprint string
}

func commentState(sync bool) string {
	if sync {
		return "pending"
	}
	return "local"
}

// ErrThreadLine: a line thread needs a path, a side and a line.
var ErrThreadLine = errors.New("a line comment needs path, side (new|old) and line")

// CreateThread starts a thread with its first comment. A conversation
// comment (empty path) goes into the change's one conversation thread.
func (s *Store) CreateThread(ctx context.Context, changeID int64, a NewThreadAnchor, c NewComment) (threadID, commentID int64, uid string, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, 0, "", err
	}
	defer tx.Rollback(ctx)
	if a.Path == "" {
		err = tx.QueryRow(ctx,
			`INSERT INTO change_threads (change_id, external_id, source, created_by) VALUES ($1, $2, 'forge', $3)
			 ON CONFLICT (change_id, external_id) WHERE external_id <> '' DO UPDATE SET updated_at = now()
			 RETURNING id`, changeID, vcs.ConversationThread, c.AuthorEmail).Scan(&threadID)
	} else {
		if (a.Side != "new" && a.Side != "old") || a.Line <= 0 || (a.StartLine != 0 && a.StartLine > a.Line) {
			return 0, 0, "", ErrThreadLine
		}
		err = tx.QueryRow(ctx,
			`INSERT INTO change_threads (change_id, path, side, line, start_line, original_line, commit_sha,
			     original_commit, anchor_head, fingerprint, source, created_by)
			 VALUES ($1,$2,$3,$4,$5,$4,$6,$6,$7,$8,'forge',$9) RETURNING id`,
			changeID, a.Path, a.Side, a.Line, a.StartLine, a.CommitSHA, a.AnchorHead, a.Fingerprint, c.AuthorEmail).Scan(&threadID)
	}
	if err != nil {
		return 0, 0, "", err
	}
	commentID, uid, err = addCommentTx(ctx, tx, threadID, c)
	if err != nil {
		return 0, 0, "", err
	}
	if err := notifyChangeTx(ctx, tx, changeID); err != nil {
		return 0, 0, "", err
	}
	return threadID, commentID, uid, tx.Commit(ctx)
}

func addCommentTx(ctx context.Context, tx pgx.Tx, threadID int64, c NewComment) (int64, string, error) {
	uid := newUID()
	var id int64
	err := tx.QueryRow(ctx,
		`INSERT INTO change_comments (thread_id, forge_uid, author, author_email, body, source, state)
		 VALUES ($1,$2,$3,$4,$5,'forge',$6) RETURNING id`,
		threadID, uid, c.Author, strings.ToLower(c.AuthorEmail), c.Body, commentState(c.Sync)).Scan(&id)
	return id, uid, err
}

func notifyChangeTx(ctx context.Context, tx pgx.Tx, changeID int64) error {
	_, err := tx.Exec(ctx, `SELECT pg_notify($1, $2)`, changesChannel, strconv.FormatInt(changeID, 10))
	return err
}

// AddReply adds a comment to a thread of changeID.
func (s *Store) AddReply(ctx context.Context, changeID, threadID int64, c NewComment) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	var ok bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM change_threads WHERE id = $1 AND change_id = $2)`,
		threadID, changeID).Scan(&ok); err != nil {
		return 0, err
	}
	if !ok {
		return 0, ErrNotFound
	}
	id, _, err := addCommentTx(ctx, tx, threadID, c)
	if err != nil {
		return 0, err
	}
	if err := notifyChangeTx(ctx, tx, changeID); err != nil {
		return 0, err
	}
	return id, tx.Commit(ctx)
}

// SetThreadResolved records a resolve/reopen made in Forge. pending marks
// that the host must be told (it then wins over refreshes until it is).
func (s *Store) SetThreadResolved(ctx context.Context, changeID, threadID int64, resolved bool, by string, pending bool) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE change_threads SET resolved = $3, resolved_by = CASE WHEN $3 THEN $4 ELSE '' END,
		     resolve_pending = $5, updated_at = now()
		 WHERE id = $1 AND change_id = $2`, threadID, changeID, resolved, by, pending)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	_, err = s.pool.Exec(ctx, `SELECT pg_notify($1, $2)`, changesChannel, strconv.FormatInt(changeID, 10))
	return err
}

// CommentToSend is what the outbox needs to post a Forge comment.
type CommentToSend struct {
	CommentID   int64
	UID         string
	Body        string
	AuthorEmail string
	Author      string
	State       string
	ExternalID  string
	ThreadID    int64
	ThreadExt   string
	RootExt     string // host id of the thread's first comment ("" until it is on the host)
	IsRoot      bool   // this comment starts the thread
	Path        string
	Side        string
	Line        int
	StartLine   int
	CommitSHA   string
	ChangeID    int64
	Repo        string
	Provider    string
	Number      string
}

// CommentForSend loads a comment and its thread for sending.
func (s *Store) CommentForSend(ctx context.Context, commentID int64) (*CommentToSend, error) {
	var c CommentToSend
	err := s.pool.QueryRow(ctx,
		`SELECT cm.id, cm.forge_uid, cm.body, cm.author_email, cm.author, cm.state, cm.external_id,
		        t.id, t.external_id, t.root_comment_ext,
		        NOT EXISTS (SELECT 1 FROM change_comments o WHERE o.thread_id = t.id AND o.id < cm.id),
		        t.path, t.side, t.line, t.start_line, t.commit_sha, ch.id, ch.repo, ch.provider, ch.number
		 FROM change_comments cm JOIN change_threads t ON t.id = cm.thread_id JOIN changes ch ON ch.id = t.change_id
		 WHERE cm.id = $1`, commentID).
		Scan(&c.CommentID, &c.UID, &c.Body, &c.AuthorEmail, &c.Author, &c.State, &c.ExternalID,
			&c.ThreadID, &c.ThreadExt, &c.RootExt, &c.IsRoot, &c.Path, &c.Side, &c.Line, &c.StartLine, &c.CommitSHA,
			&c.ChangeID, &c.Repo, &c.Provider, &c.Number)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &c, err
}

// MarkCommentSent links a Forge comment to its host copy. The first comment
// of a thread also gives the thread its root (replies go to it).
func (s *Store) MarkCommentSent(ctx context.Context, c *CommentToSend, externalID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx,
		`UPDATE change_comments SET external_id = $2, state = 'sent', error = '', updated_at = now() WHERE id = $1`,
		c.CommentID, externalID); err != nil {
		return err
	}
	if c.IsRoot && c.Path != "" {
		if _, err := tx.Exec(ctx,
			`UPDATE change_threads SET root_comment_ext = $2 WHERE id = $1 AND root_comment_ext = ''`, c.ThreadID, externalID); err != nil {
			return err
		}
	}
	if err := notifyChangeTx(ctx, tx, c.ChangeID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// MarkCommentFailed keeps why a comment could not be posted (it stays in
// Forge, marked, for the author to retry or copy).
func (s *Store) MarkCommentFailed(ctx context.Context, c *CommentToSend, reason string) error {
	if _, err := s.pool.Exec(ctx,
		`UPDATE change_comments SET state = 'failed', error = $2, updated_at = now() WHERE id = $1`,
		c.CommentID, truncate(reason, 300)); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `SELECT pg_notify($1, $2)`, changesChannel, strconv.FormatInt(c.ChangeID, 10))
	return err
}

// ThreadForResolve returns a thread's host id and resolved flag, and its
// change's number, repo and provider.
func (s *Store) ThreadForResolve(ctx context.Context, threadID int64) (ext string, resolved bool, number, repo, provider string, err error) {
	err = s.pool.QueryRow(ctx,
		`SELECT t.external_id, t.resolved, ch.number, ch.repo, ch.provider FROM change_threads t JOIN changes ch ON ch.id = t.change_id
		 WHERE t.id = $1`, threadID).Scan(&ext, &resolved, &number, &repo, &provider)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return
}

// ClearResolvePending: the host now knows the thread's resolved state.
func (s *Store) ClearResolvePending(ctx context.Context, threadID int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE change_threads SET resolve_pending = FALSE WHERE id = $1`, threadID)
	return err
}

// ---- re-anchoring (hosts without native threads) ----

// AnchoredThread is a Forge line thread whose change moved on since it was
// placed.
type AnchoredThread struct {
	ID          int64
	ChangeID    int64
	Repo        string
	Provider    string
	Number      string
	Source      string
	Target      string
	Path        string
	Side        string
	Line        int
	AnchorHead  string
	Head        string
	Fingerprint string
}

// ThreadsToReanchor lists open line threads whose change has a new head,
// for providers in providers (those without native threads).
func (s *Store) ThreadsToReanchor(ctx context.Context, providers []string, limit int) ([]AnchoredThread, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT t.id, t.change_id, ch.repo, ch.provider, ch.number, ch.source_branch, ch.target_branch,
		        t.path, t.side, t.line, t.anchor_head, ch.head_sha, t.fingerprint
		 FROM change_threads t JOIN changes ch ON ch.id = t.change_id
		 WHERE ch.state = 'open' AND ch.provider = ANY($1) AND t.path <> '' AND NOT t.outdated
		   AND t.anchor_head <> '' AND t.anchor_head <> ch.head_sha
		 ORDER BY t.id LIMIT $2`, providers, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AnchoredThread
	for rows.Next() {
		var a AnchoredThread
		if err := rows.Scan(&a.ID, &a.ChangeID, &a.Repo, &a.Provider, &a.Number, &a.Source, &a.Target,
			&a.Path, &a.Side, &a.Line, &a.AnchorHead, &a.Head, &a.Fingerprint); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Reanchor moves a thread to line on head, or marks it outdated (line 0).
func (s *Store) Reanchor(ctx context.Context, a AnchoredThread, line int) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE change_threads SET line = $2, anchor_head = $3, commit_sha = $3, outdated = ($2 = 0), updated_at = now()
		 WHERE id = $1`, a.ID, line, a.Head)
	if err == nil {
		_, err = s.pool.Exec(ctx, `SELECT pg_notify($1, $2)`, changesChannel, strconv.FormatInt(a.ChangeID, 10))
	}
	return err
}

// ---- linked host accounts ----

// HostLink is a host account a Forge user linked.
type HostLink struct {
	Provider string    `json:"provider"`
	Login    string    `json:"login"`
	Scopes   string    `json:"scopes"`
	Created  time.Time `json:"created_at"`
}

// LinkHostAccount stores a user's host token (encrypted) and maps the login
// to them as a verified identity.
func (s *Store) LinkHostAccount(ctx context.Context, email, provider, login, token, scopes string) error {
	enc, err := s.cipher.Encrypt(token)
	if err != nil {
		return err
	}
	email, login = strings.ToLower(email), strings.ToLower(login)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx,
		`INSERT INTO user_host_tokens (email, provider, login, token, scopes) VALUES ($1,$2,$3,$4,$5)
		 ON CONFLICT (email, provider) DO UPDATE SET login = EXCLUDED.login, token = EXCLUDED.token,
		     scopes = EXCLUDED.scopes, created_at = now()`, email, provider, login, enc, scopes); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO host_identities (provider, login, email, verified) VALUES ($1,$2,$3,TRUE)
		 ON CONFLICT (provider, login) DO UPDATE SET email = EXCLUDED.email, verified = TRUE`, provider, login, email); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE changes SET gate_dirty_at = now() WHERE state = 'open'`); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// HostLinks lists a user's linked accounts (never the tokens).
func (s *Store) HostLinks(ctx context.Context, email string) ([]HostLink, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT provider, login, scopes, created_at FROM user_host_tokens WHERE email = $1 ORDER BY provider`,
		strings.ToLower(email))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []HostLink{}
	for rows.Next() {
		var l HostLink
		if err := rows.Scan(&l.Provider, &l.Login, &l.Scopes, &l.Created); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// UnlinkHostAccount forgets a user's token (the login mapping stays, as an
// unverified identity an admin can remove).
func (s *Store) UnlinkHostAccount(ctx context.Context, email, provider string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM user_host_tokens WHERE email = $1 AND provider = $2`,
		strings.ToLower(email), provider)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	_, err = s.pool.Exec(ctx, `UPDATE host_identities SET verified = FALSE WHERE provider = $1 AND email = $2`,
		provider, strings.ToLower(email))
	return err
}

// UserHostToken returns a user's own token for provider (ok=false: not linked).
func (s *Store) UserHostToken(ctx context.Context, email, provider string) (login, token string, ok bool, err error) {
	var enc string
	err = s.pool.QueryRow(ctx, `SELECT login, token FROM user_host_tokens WHERE email = $1 AND provider = $2`,
		strings.ToLower(email), provider).Scan(&login, &enc)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	token, err = s.cipher.Decrypt(enc)
	return login, token, err == nil, err
}
