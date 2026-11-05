package review

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/priytamjeepandey/forge-ci/internal/store"
)

// Code owners: the CODEOWNERS file on the target branch (GitHub, GitLab and
// Bitbucket syntax) names who owns which paths; with the rule on, someone
// who owns each touched file must approve. Forge enforces it on every host,
// including hosts that have no code owners of their own.

// OwnerRule is one CODEOWNERS line: a gitignore-style pattern and its owners
// (@login, @org/team or an email). No owners means "nobody owns this".
type OwnerRule struct {
	Pattern string
	Owners  []string
	re      *regexp.Regexp
}

// ParseCodeOwners reads a CODEOWNERS file. Comments, blank lines and GitLab
// section headers are skipped; a pattern that cannot be read is reported and
// left out.
func ParseCodeOwners(content string) ([]OwnerRule, []string) {
	var rules []OwnerRule
	var bad []string
	for n, line := range strings.Split(content, "\n") {
		if i := strings.Index(line, " #"); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "[") || strings.HasPrefix(line, "^[") {
			continue
		}
		f := strings.Fields(line)
		re, err := ownerPattern(f[0])
		if err != nil {
			bad = append(bad, fmt.Sprintf("line %d: %s", n+1, f[0]))
			continue
		}
		owners := make([]string, 0, len(f)-1)
		for _, o := range f[1:] {
			owners = append(owners, strings.ToLower(o))
		}
		rules = append(rules, OwnerRule{Pattern: f[0], Owners: owners, re: re})
	}
	return rules, bad
}

// ownerPattern turns a gitignore-style pattern into a regexp over a file
// path. A pattern with a slash (other than at the end) is anchored at the
// root; one without matches at any depth. A pattern names a file or a
// directory, and a directory owns everything below it — except that a
// wildcard in the last segment stays at that level, as on GitHub.
func ownerPattern(p string) (*regexp.Regexp, error) {
	dirOnly := strings.HasSuffix(p, "/")
	p = strings.TrimSuffix(p, "/")
	anchored := strings.HasPrefix(p, "/") || strings.Contains(p, "/")
	p = strings.TrimPrefix(p, "/")
	if p == "" || p == "*" && !anchored && !dirOnly {
		return regexp.Compile(`^.*$`)
	}
	var b strings.Builder
	if anchored {
		b.WriteString("^")
	} else {
		b.WriteString("^(?:.*/)?")
	}
	for i := 0; i < len(p); i++ {
		switch c := p[i]; {
		case strings.HasPrefix(p[i:], "**/"):
			b.WriteString("(?:.*/)?")
			i += 2
		case strings.HasPrefix(p[i:], "/**") && i+3 == len(p):
			b.WriteString("/.*")
			i += 2
		case strings.HasPrefix(p[i:], "**"):
			b.WriteString(".*")
			i++
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?':
			b.WriteString("[^/]")
		case c == '\\' && i+1 < len(p):
			i++
			b.WriteString(regexp.QuoteMeta(string(p[i])))
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	last := p[strings.LastIndex(p, "/")+1:]
	switch {
	case dirOnly:
		b.WriteString("/.*$")
	case strings.Contains(last, "*") && last != "**":
		// As GitHub reads it: "docs/*" owns docs/a.md, not docs/deep/a.md.
		b.WriteString("$")
	default:
		b.WriteString("(?:/.*)?$")
	}
	return regexp.Compile(b.String())
}

// OwnersOf returns the owners of path: the last matching rule wins.
func OwnersOf(rules []OwnerRule, path string) (owners []string, matched bool) {
	for i := len(rules) - 1; i >= 0; i-- {
		if rules[i].re.MatchString(path) {
			return rules[i].Owners, true
		}
	}
	return nil, false
}

// approver is someone whose approval counted, as the owner rule matches
// them: their Forge email and, for a host review, their host login.
type approver struct {
	email string
	login string
}

// ownerResolver says whether an owner token names an approver.
type ownerResolver struct {
	identities map[string]string          // host login → email
	groups     map[string]map[string]bool // Forge approver group → members
}

// members returns who owner names in Forge (emails), and whether it names
// anyone at all.
func (r ownerResolver) members(owner string) (map[string]bool, bool) {
	out := map[string]bool{}
	switch {
	case strings.Contains(owner, "@") && !strings.HasPrefix(owner, "@"):
		out[owner] = true
	case strings.Contains(owner, "/"): // @org/team: a Forge group by either name
		name := strings.TrimPrefix(owner, "@")
		g, ok := r.groups[name]
		if !ok {
			_, team, _ := strings.Cut(name, "/")
			g, ok = r.groups[team]
		}
		if !ok {
			return out, false
		}
		for m := range g {
			out[m] = true
		}
	default: // @login, else a Forge group of that name
		name := strings.TrimPrefix(owner, "@")
		if e := r.identities[name]; e != "" {
			out[e] = true
		}
		for m := range r.groups[name] {
			out[m] = true
		}
	}
	return out, len(out) > 0
}

func (r ownerResolver) approves(owner string, a approver) bool {
	if a.login != "" && owner == "@"+a.login {
		return true
	}
	m, _ := r.members(owner)
	return a.email != "" && m[a.email]
}

// codeOwnersRule evaluates the code owners rule for the files in co.
func codeOwnersRule(co *store.CodeOwnersInput, target string, res ownerResolver, approvers []approver) (string, string) {
	switch {
	case co == nil || co.State == "pending":
		return "pending", "Working out which files the change touches"
	case co.State == "failed":
		return "pending", "Could not read the change's files: " + co.Error
	case !co.OwnersRead:
		return "pending", "A runner must be upgraded to read CODEOWNERS"
	case co.File == "":
		return "pass", "No CODEOWNERS file on " + target
	}
	rules, bad := ParseCodeOwners(co.Content)
	// Group the touched files by the owner set that must approve them.
	type need struct {
		owners []string
		paths  []string
	}
	needs := map[string]*need{}
	var keys []string
	owned := 0
	for _, p := range co.Paths {
		owners, ok := OwnersOf(rules, p)
		if !ok || len(owners) == 0 {
			continue
		}
		owned++
		k := strings.Join(owners, " ")
		if needs[k] == nil {
			needs[k] = &need{owners: owners}
			keys = append(keys, k)
		}
		needs[k].paths = append(needs[k].paths, p)
	}
	sort.Strings(keys)
	var waiting, unknown []string
	for _, k := range keys {
		n := needs[k]
		ok, anyone := false, false
		for _, o := range n.owners {
			if _, named := res.members(o); named {
				anyone = true
			}
			for _, a := range approvers {
				if res.approves(o, a) {
					ok = true
				}
			}
		}
		if ok {
			continue
		}
		files := n.paths[0]
		if len(n.paths) > 1 {
			files += fmt.Sprintf(" and %d more", len(n.paths)-1)
		}
		waiting = append(waiting, strings.Join(n.owners, " or ")+" for "+files)
		if !anyone {
			unknown = append(unknown, n.owners...)
		}
	}
	if len(waiting) == 0 {
		if owned == 0 {
			return "pass", "No touched file has a code owner"
		}
		return "pass", fmt.Sprintf("Code owners approved all %d owned files", owned)
	}
	detail := "Needs approval from " + strings.Join(waiting, "; ")
	if len(unknown) > 0 {
		detail += " (no Forge user for " + strings.Join(unknown, ", ") + " — map host logins or create the group in Admin)"
	}
	if len(bad) > 0 {
		detail += "; ignored CODEOWNERS " + strings.Join(bad, ", ")
	}
	return "pending", detail
}
