package compiler

import (
	"regexp"
	"strings"
	"sync"
)

// ChangeSet is the list of files a pipeline's commit changed, against Base
// (the last successful pipeline on the branch, or the merge base for a new
// branch / merge request). A nil *ChangeSet means "unknown": every
// rules: changes: clause is then satisfied, so jobs run rather than being
// skipped on a guess.
type ChangeSet struct {
	Base  string
	Files []string
}

var (
	globCacheMu sync.Mutex
	globCache   = map[string]*regexp.Regexp{}
)

// globRegexp compiles a GitLab-style path glob: `**` matches any number of
// path segments, `*` anything within one segment, `?` one character, and
// `{a,b}` alternatives. A pattern ending in `/` matches everything under that
// directory.
func globRegexp(pattern string) *regexp.Regexp {
	globCacheMu.Lock()
	defer globCacheMu.Unlock()
	if re, ok := globCache[pattern]; ok {
		return re
	}
	p := strings.TrimPrefix(pattern, "./")
	if strings.HasSuffix(p, "/") {
		p += "**"
	}
	var b strings.Builder
	b.WriteString("^")
	depth := 0
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case c == '*' && i+1 < len(p) && p[i+1] == '*':
			i++
			if i+1 < len(p) && p[i+1] == '/' {
				i++
				b.WriteString("(?:.*/)?") // **/ — zero or more directories
			} else {
				b.WriteString(".*")
			}
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?':
			b.WriteString("[^/]")
		case c == '{':
			depth++
			b.WriteString("(?:")
		case c == '}' && depth > 0:
			depth--
			b.WriteString(")")
		case c == ',' && depth > 0:
			b.WriteString("|")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		re = regexp.MustCompile("^" + regexp.QuoteMeta(pattern) + "$")
	}
	globCache[pattern] = re
	return re
}

// matchesChanges reports whether any changed file matches any pattern.
func (c *ChangeSet) matchesChanges(patterns []string) bool {
	if c == nil {
		return true
	}
	for _, pat := range patterns {
		re := globRegexp(pat)
		for _, f := range c.Files {
			if re.MatchString(f) {
				return true
			}
		}
	}
	return false
}
