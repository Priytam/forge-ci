package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/priytamjeepandey/forge-ci/internal/live"
)

// Forge Live templates (phase D): one application declared on many targets,
// the ApplicationSet of Forge. Agents fetch what their target's templates
// declare (LiveDeclaredApps) and render it before anything of it exists.

// LiveTemplate declares an application on every target it matches.
type LiveTemplate struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	AppName string `json:"app_name"`
	// Namespace and Path may use {target}, {environment} and {name}.
	Namespace       string    `json:"namespace"`
	Group           string    `json:"group"`
	Path            string    `json:"path"`
	Targets         []string  `json:"targets"`      // by name; none = any
	Environments    []string  `json:"environments"` // none = any
	CreateNamespace bool      `json:"create_namespace"`
	AutoSync        bool      `json:"auto_sync"`
	Prune           bool      `json:"prune"`
	SelfHeal        bool      `json:"self_heal"`
	CreatedBy       string    `json:"created_by"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// ErrDuplicateTemplate: a template of that name exists.
var ErrDuplicateTemplate = errors.New("a template with that name exists")

var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// Validate checks a template before it is saved.
func (t *LiveTemplate) Validate() error {
	t.Name, t.AppName, t.Path = strings.TrimSpace(t.Name), strings.TrimSpace(t.AppName), strings.Trim(strings.TrimSpace(t.Path), "/")
	if t.AppName == "" {
		t.AppName = t.Name
	}
	switch {
	case !dnsLabel.MatchString(t.Name):
		return fmt.Errorf("name: lowercase letters, digits and dashes")
	case !dnsLabel.MatchString(t.AppName):
		return fmt.Errorf("app name: lowercase letters, digits and dashes")
	case t.Namespace == "":
		return fmt.Errorf("namespace is required")
	case t.Path == "" || strings.Contains(t.Path, ".."):
		return fmt.Errorf("path: a directory in the targets' source")
	case t.SelfHeal && !t.AutoSync, t.Prune && !t.AutoSync:
		return fmt.Errorf("prune and self-heal are part of auto-sync: turn it on too")
	}
	if t.Targets == nil {
		t.Targets = []string{}
	}
	if t.Environments == nil {
		t.Environments = []string{}
	}
	return nil
}

// matches says whether t declares its app on target.
func (t *LiveTemplate) matches(target *LiveTarget) bool {
	return (len(t.Targets) == 0 || slices.Contains(t.Targets, target.Name)) &&
		(len(t.Environments) == 0 || slices.Contains(t.Environments, target.Environment))
}

// declaredOn is t's application on target.
func (t *LiveTemplate) declaredOn(target *LiveTarget) live.DeclaredApp {
	fill := strings.NewReplacer("{target}", target.Name, "{environment}", target.Environment, "{name}", t.AppName).Replace
	ns := fill(t.Namespace)
	return live.DeclaredApp{
		App:             live.App{Key: ns + "/" + t.AppName, Name: t.AppName, Namespace: ns, Group: t.Group, Template: t.Name},
		Path:            fill(t.Path),
		CreateNamespace: t.CreateNamespace,
	}
}

const liveTemplateCols = `id, name, app_name, namespace, app_group, path, targets, environments, create_namespace,
	auto_sync, prune, self_heal, created_by, created_at, updated_at`

func scanLiveTemplate(row pgx.Row) (*LiveTemplate, error) {
	var t LiveTemplate
	err := row.Scan(&t.ID, &t.Name, &t.AppName, &t.Namespace, &t.Group, &t.Path, &t.Targets, &t.Environments,
		&t.CreateNamespace, &t.AutoSync, &t.Prune, &t.SelfHeal, &t.CreatedBy, &t.CreatedAt, &t.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &t, err
}

// LiveTemplates lists the templates by name.
func (s *Store) LiveTemplates(ctx context.Context) ([]LiveTemplate, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+liveTemplateCols+` FROM live_templates ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LiveTemplate{}
	for rows.Next() {
		t, err := scanLiveTemplate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// LiveTemplate returns one template.
func (s *Store) LiveTemplate(ctx context.Context, id int64) (*LiveTemplate, error) {
	return scanLiveTemplate(s.pool.QueryRow(ctx, `SELECT `+liveTemplateCols+` FROM live_templates WHERE id = $1`, id))
}

// SaveLiveTemplate creates t (ID 0) or updates it. An update re-applies
// its sync policy to the apps whose policy the template set and nobody has
// changed since.
func (s *Store) SaveLiveTemplate(ctx context.Context, t *LiveTemplate, by string) error {
	if err := t.Validate(); err != nil {
		return err
	}
	var row pgx.Row
	if t.ID == 0 {
		row = s.pool.QueryRow(ctx, `INSERT INTO live_templates (name, app_name, namespace, app_group, path, targets, environments,
			create_namespace, auto_sync, prune, self_heal, created_by) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
			RETURNING `+liveTemplateCols, t.Name, t.AppName, t.Namespace, t.Group, t.Path, t.Targets, t.Environments,
			t.CreateNamespace, t.AutoSync, t.Prune, t.SelfHeal, by)
	} else {
		row = s.pool.QueryRow(ctx, `UPDATE live_templates SET name=$2, app_name=$3, namespace=$4, app_group=$5, path=$6, targets=$7,
			environments=$8, create_namespace=$9, auto_sync=$10, prune=$11, self_heal=$12, updated_at=now() WHERE id=$1
			RETURNING `+liveTemplateCols, t.ID, t.Name, t.AppName, t.Namespace, t.Group, t.Path, t.Targets, t.Environments,
			t.CreateNamespace, t.AutoSync, t.Prune, t.SelfHeal)
	}
	saved, err := scanLiveTemplate(row)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrDuplicateTemplate
	}
	if err != nil {
		return err
	}
	*t = *saved
	_, err = s.pool.Exec(ctx, `UPDATE live_app_policies SET auto_sync=$2, prune=$3, self_heal=$4, updated_at=now()
		WHERE updated_by = $1`, templatePolicyActor(t.Name), t.AutoSync, t.Prune, t.SelfHeal)
	return err
}

// DeleteLiveTemplate removes a template. Its apps stay in the clusters (and
// in Forge while anything of them exists); nothing declares them any more.
func (s *Store) DeleteLiveTemplate(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM live_templates WHERE id = $1`, id)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

func templatePolicyActor(name string) string { return "template " + name }

// LiveDeclaredApps are the applications target's templates declare. Each
// gets its template's sync policy the first time (never overriding one).
func (s *Store) LiveDeclaredApps(ctx context.Context, target *LiveTarget) ([]live.DeclaredApp, error) {
	ts, err := s.LiveTemplates(ctx)
	if err != nil {
		return nil, err
	}
	out := []live.DeclaredApp{}
	for i := range ts {
		t := &ts[i]
		if !t.matches(target) {
			continue
		}
		d := t.declaredOn(target)
		out = append(out, d)
		if _, err := s.pool.Exec(ctx, `INSERT INTO live_app_policies (target_id, app_key, auto_sync, prune, self_heal, updated_by)
			VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (target_id, app_key) DO NOTHING`,
			target.ID, d.App.Key, t.AutoSync, t.Prune, t.SelfHeal, templatePolicyActor(t.Name)); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ---- the fleet ----

// FleetCell is one application on one target.
type FleetCell struct {
	AppID    int64    `json:"app_id"`
	Health   string   `json:"health"`
	Sync     string   `json:"sync"`
	Revision string   `json:"revision"`
	Images   []string `json:"images"`
	Pods     int      `json:"pods"`
	Ready    int      `json:"pods_ready"`
}

// FleetRow is one application name across targets.
type FleetRow struct {
	Name     string              `json:"name"`
	Group    string              `json:"group"`
	Template string              `json:"template"`
	Cells    map[int64]FleetCell `json:"cells"` // by target id
}

// FleetQuery narrows the fleet view.
type FleetQuery struct {
	Environment, Template, Search string
	Limit, Offset                 int
}

// LiveFleet returns a page of applications (by name) across every target,
// the number of names, and the targets that are columns.
func (s *Store) LiveFleet(ctx context.Context, q FleetQuery) ([]FleetRow, int, []LiveTarget, error) {
	where := []string{"NOT a.orphans"}
	args := []any{}
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}
	if q.Environment != "" {
		add("t.environment = $%d", q.Environment)
	}
	if q.Template != "" {
		add("a.template = $%d", q.Template)
	}
	if q.Search != "" {
		add("(a.name ILIKE '%%' || $%d || '%%')", escapeLike(q.Search))
	}
	w := strings.Join(where, " AND ")
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(DISTINCT a.name)`+liveAppFrom+` WHERE `+w, args...).Scan(&total); err != nil {
		return nil, 0, nil, err
	}
	rows, err := s.pool.Query(ctx, `WITH page AS (SELECT DISTINCT a.name`+liveAppFrom+` WHERE `+w+
		fmt.Sprintf(` ORDER BY a.name LIMIT %d OFFSET %d)`, q.Limit, q.Offset)+`
		SELECT a.id, a.name, a.app_group, a.template, a.target_id, a.health, a.sync, a.revision, a.images, a.pods, a.pods_ready`+
		liveAppFrom+` JOIN page ON page.name = a.name WHERE `+w+` ORDER BY a.name, a.target_id`, args...)
	if err != nil {
		return nil, 0, nil, err
	}
	defer rows.Close()
	out := []FleetRow{}
	for rows.Next() {
		var c FleetCell
		var name, group, template string
		var target int64
		if err := rows.Scan(&c.AppID, &name, &group, &template, &target, &c.Health, &c.Sync, &c.Revision, &c.Images, &c.Pods, &c.Ready); err != nil {
			return nil, 0, nil, err
		}
		if len(out) == 0 || out[len(out)-1].Name != name {
			out = append(out, FleetRow{Name: name, Group: group, Template: template, Cells: map[int64]FleetCell{}})
		}
		r := &out[len(out)-1]
		if r.Template == "" {
			r.Template = template
		}
		if _, dup := r.Cells[target]; !dup { // the same name in two namespaces of one target: the first
			r.Cells[target] = c
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, nil, err
	}
	targets, err := s.LiveTargets(ctx)
	if err != nil {
		return nil, 0, nil, err
	}
	cols := targets[:0]
	for _, t := range targets {
		if q.Environment == "" || t.Environment == q.Environment {
			cols = append(cols, t)
		}
	}
	return out, total, cols, nil
}
