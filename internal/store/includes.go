package store

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/priytamjeepandey/forge-ci/internal/compiler"
	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

// Config reuse beyond a repo's own templates: files from other connected
// repos (any git host), remote URLs and the org template catalog. The
// compiler asks through an IncludeFunc; everything that touches the network
// or the database lives here.

// ErrVersionExists is returned when publishing a catalog version that exists.
var ErrVersionExists = errors.New("this template version is already published (versions are immutable; publish a new one)")

// CompileOpts returns the compile options for a repo: its own templates plus
// project/remote/catalog includes resolved on its behalf.
func (s *Store) CompileOpts(ctx context.Context, repo string) compiler.CompileOpts {
	return compiler.CompileOpts{Templates: s.TemplateResolver(ctx, repo), Includes: s.IncludeResolver(ctx, repo)}
}

// IncludeResolver resolves external includes for pipelines of repo.
func (s *Store) IncludeResolver(ctx context.Context, repo string) compiler.IncludeFunc {
	return func(ref compiler.IncludeRef) (string, bool, error) {
		switch ref.Kind {
		case "project":
			return s.projectInclude(ctx, repo, ref)
		case "remote":
			body, err := fetchRemoteInclude(ctx, ref.URL, ref.Integrity)
			return body, err == nil, err
		case "catalog":
			t, err := s.GetCatalogTemplate(ctx, ref.Name, ref.Version)
			if errors.Is(err, ErrNotFound) {
				return "", false, nil
			}
			if err != nil {
				return "", false, err
			}
			return t.YAML, true, nil
		}
		return "", false, fmt.Errorf("unknown include kind %q", ref.Kind)
	}
}

// ---- project: includes ----

// mayIncludeFrom decides whether repo's pipelines may read files from
// project. Reads use Forge's own credential for project, so without a rule
// any repo could read any other connected private repo. Allowed: the same
// repo, the same owner/namespace (first path segment), or a project listed in
// FORGE_INCLUDE_SHARED_PROJECTS (comma-separated; a trailing /* matches a
// namespace; * allows every connected repo).
func mayIncludeFrom(repo, project string) bool {
	if repo == project {
		return true
	}
	owner := func(r string) string { o, _, _ := strings.Cut(r, "/"); return o }
	if strings.Contains(repo, "/") && owner(repo) == owner(project) {
		return true
	}
	for _, p := range strings.Split(os.Getenv("FORGE_INCLUDE_SHARED_PROJECTS"), ",") {
		p = strings.TrimSpace(p)
		switch {
		case p == "":
		case p == "*", p == project:
			return true
		case strings.HasSuffix(p, "/*") && strings.HasPrefix(project, strings.TrimSuffix(p, "*")):
			return true
		}
	}
	return false
}

var shaRe = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

func (s *Store) projectInclude(ctx context.Context, repo string, ref compiler.IncludeRef) (string, bool, error) {
	if !mayIncludeFrom(repo, ref.Project) {
		return "", false, fmt.Errorf("%s may not include files from %s (different owner; an admin can share it with FORGE_INCLUDE_SHARED_PROJECTS)", repo, ref.Project)
	}
	if ref.Ref == "" {
		return "", false, fmt.Errorf("project include of %s needs ref: (a tag, branch or commit; pin a tag or commit for reproducible builds)", ref.Project)
	}
	if _, registered, err := s.resolveRepoAuth(ctx, ref.Project); err != nil {
		return "", false, err
	} else if !registered {
		return "", false, fmt.Errorf("%s is not a connected repository", ref.Project)
	}
	sha := ref.Ref
	if !shaRe.MatchString(sha) {
		resolved, err := s.ResolveRef(ctx, ref.Project, ref.Ref)
		if err != nil {
			return "", false, fmt.Errorf("resolving %s@%s: %w", ref.Project, ref.Ref, err)
		}
		sha = resolved
	}
	return s.ConfigFromRepo(ctx, ref.Project, sha, ref.File)
}

// ---- remote: includes ----

const maxRemoteInclude = 1 << 20 // 1 MiB of YAML is plenty

// remoteClient fetches remote includes. Every connection is checked at dial
// time (so DNS rebinding can't sneak past) against private, loopback,
// link-local and other internal ranges unless FORGE_INCLUDE_REMOTE_ALLOW_PRIVATE
// is true; redirects must stay on https.
var remoteClient = &http.Client{
	Timeout: 15 * time.Second,
	Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout: 10 * time.Second,
			Control: func(_, address string, _ syscall.RawConn) error {
				host, _, err := net.SplitHostPort(address)
				if err != nil {
					return err
				}
				ip := net.ParseIP(host)
				if ip == nil {
					return fmt.Errorf("remote include: unexpected address %s", address)
				}
				if blockedIP(ip) && os.Getenv("FORGE_INCLUDE_REMOTE_ALLOW_PRIVATE") != "true" {
					return fmt.Errorf("remote include: %s is an internal address (blocked)", ip)
				}
				return nil
			},
		}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
	},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" {
			return errors.New("remote include: redirect away from https")
		}
		if len(via) >= 3 {
			return errors.New("remote include: too many redirects")
		}
		return nil
	},
}

func blockedIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast() {
		return true
	}
	// Carrier-grade NAT (100.64/10), where cloud metadata and VPC services can live.
	_, cgnat, _ := net.ParseCIDR("100.64.0.0/10")
	return cgnat.Contains(ip)
}

// remoteHostAllowed applies FORGE_INCLUDE_REMOTE_HOSTS (comma-separated host
// names; *.example.com matches subdomains). Unset allows any public host.
func remoteHostAllowed(host string) bool {
	list := strings.TrimSpace(os.Getenv("FORGE_INCLUDE_REMOTE_HOSTS"))
	if list == "" {
		return true
	}
	host = strings.ToLower(host)
	for _, h := range strings.Split(list, ",") {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == host || (strings.HasPrefix(h, "*.") && strings.HasSuffix(host, h[1:])) {
			return true
		}
	}
	return false
}

func fetchRemoteInclude(ctx context.Context, raw, integrity string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("invalid URL %q", raw)
	}
	if u.Scheme != "https" {
		return "", errors.New("remote includes must use https://")
	}
	if u.User != nil {
		return "", errors.New("remote include URLs may not embed credentials")
	}
	if !remoteHostAllowed(u.Hostname()) {
		return "", fmt.Errorf("host %s is not in FORGE_INCLUDE_REMOTE_HOSTS", u.Hostname())
	}
	if integrity == "" && os.Getenv("FORGE_INCLUDE_REMOTE_REQUIRE_INTEGRITY") == "true" {
		return "", errors.New("this server requires integrity: on remote includes")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	resp, err := remoteClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET returned %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRemoteInclude+1))
	if err != nil {
		return "", err
	}
	if len(body) > maxRemoteInclude {
		return "", errors.New("remote include is larger than 1 MiB")
	}
	if integrity != "" {
		if err := checkIntegrity(body, integrity); err != nil {
			return "", err
		}
	}
	return string(body), nil
}

// checkIntegrity accepts sha256-<base64> (SRI style) or sha256:<hex>.
func checkIntegrity(body []byte, want string) error {
	sum := sha256.Sum256(body)
	switch {
	case strings.HasPrefix(want, "sha256-"):
		if base64.StdEncoding.EncodeToString(sum[:]) == strings.TrimPrefix(want, "sha256-") {
			return nil
		}
	case strings.HasPrefix(want, "sha256:"):
		if hex.EncodeToString(sum[:]) == strings.ToLower(strings.TrimPrefix(want, "sha256:")) {
			return nil
		}
	default:
		return errors.New("integrity must be sha256-<base64> or sha256:<hex>")
	}
	return fmt.Errorf("integrity mismatch: content is sha256:%s", hex.EncodeToString(sum[:]))
}

// ---- template catalog ----

var catalogNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*(/[a-z0-9][a-z0-9._-]*)*$`)
var catalogVersionRe = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+-]*$`)

// ValidateCatalogEntry checks name and version shape and that the YAML parses
// as a template (including its spec: inputs: header).
func ValidateCatalogEntry(name, version, yml string) error {
	if !catalogNameRe.MatchString(name) || len(name) > 120 {
		return errors.New("name must be lowercase path segments, e.g. deploy/kubernetes")
	}
	if !catalogVersionRe.MatchString(version) || version == "latest" || len(version) > 40 {
		return errors.New(`version must look like 1.2.0 (and may not be "latest")`)
	}
	if strings.TrimSpace(yml) == "" {
		return errors.New("yaml is empty")
	}
	return compiler.ValidateTemplate(yml)
}

func (s *Store) PublishCatalogTemplate(ctx context.Context, t proto.CatalogTemplate) error {
	if err := ValidateCatalogEntry(t.Name, t.Version, t.YAML); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO template_catalog (name, version, yaml, description, published_by) VALUES ($1,$2,$3,$4,$5)`,
		t.Name, t.Version, t.YAML, t.Description, t.PublishedBy)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrVersionExists
	}
	return err
}

// ListCatalog returns every version, newest version first within each name.
func (s *Store) ListCatalog(ctx context.Context) ([]proto.CatalogTemplate, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT name, version, yaml, description, published_by, created_at FROM template_catalog ORDER BY name`)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (proto.CatalogTemplate, error) {
		var t proto.CatalogTemplate
		err := r.Scan(&t.Name, &t.Version, &t.YAML, &t.Description, &t.PublishedBy, &t.CreatedAt)
		t.Inputs = []proto.TemplateInput{}
		for _, in := range compiler.TemplateInputs(t.YAML) {
			t.Inputs = append(t.Inputs, proto.TemplateInput(in))
		}
		return t, err
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return compareVersions(out[i].Version, out[j].Version) > 0
	})
	return out, nil
}

// GetCatalogTemplate returns one version; "" or "latest" is the newest.
func (s *Store) GetCatalogTemplate(ctx context.Context, name, version string) (proto.CatalogTemplate, error) {
	all, err := s.ListCatalog(ctx)
	if err != nil {
		return proto.CatalogTemplate{}, err
	}
	for _, t := range all {
		if t.Name == name && (version == "" || version == "latest" || t.Version == version) {
			return t, nil
		}
	}
	return proto.CatalogTemplate{}, ErrNotFound
}

func (s *Store) DeleteCatalogTemplate(ctx context.Context, name, version string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM template_catalog WHERE name=$1 AND version=$2`, name, version)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// compareVersions orders dotted versions numerically where possible
// (1.10.0 > 1.9.2), falling back to string order per segment; a pre-release
// (1.0.0-rc1) sorts before its release.
func compareVersions(a, b string) int {
	a = strings.TrimPrefix(a, "v")
	b = strings.TrimPrefix(b, "v")
	ac, apre, _ := strings.Cut(a, "-")
	bc, bpre, _ := strings.Cut(b, "-")
	as, bs := strings.Split(ac, "."), strings.Split(bc, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		x, y := "0", "0" // 1.2 == 1.2.0
		if i < len(as) {
			x = as[i]
		}
		if i < len(bs) {
			y = bs[i]
		}
		xn, xerr := strconv.Atoi(x)
		yn, yerr := strconv.Atoi(y)
		switch {
		case xerr == nil && yerr == nil && xn != yn:
			if xn < yn {
				return -1
			}
			return 1
		case (xerr != nil || yerr != nil) && x != y:
			return strings.Compare(x, y)
		}
	}
	switch {
	case apre == bpre:
		return 0
	case apre == "":
		return 1
	case bpre == "":
		return -1
	}
	return strings.Compare(apre, bpre)
}
