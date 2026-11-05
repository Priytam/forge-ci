package compiler

import (
	"fmt"
	"regexp"
	"strings"
)

// bashisms are bash-only constructs that sh (dash, BusyBox ash) rejects,
// often only when the line runs ("Bad substitution").
var bashisms = []struct {
	re   *regexp.Regexp
	what string
}{
	{regexp.MustCompile(`\$\{[A-Za-z_][A-Za-z0-9_]*:( -|[0-9$])`), "${VAR:offset:length} (substring)"},
	{regexp.MustCompile(`\$\{[A-Za-z_][A-Za-z0-9_]*//?[^}]*/`), "${VAR/pattern/replacement}"},
	{regexp.MustCompile(`\$\{[A-Za-z_][A-Za-z0-9_]*(\^|,)`), "${VAR^^} / ${VAR,,} (case change)"},
	{regexp.MustCompile(`(^|[\s;&|(!])\[\[\s`), "[[ … ]]"},
	{regexp.MustCompile(`(^|\s)[<>]\(`), "<(…) process substitution"},
	{regexp.MustCompile(`(^|[\s;&|])[A-Za-z_][A-Za-z0-9_]*=\(`), "arrays (name=(…))"},
	{regexp.MustCompile(`\$\{[A-Za-z_][A-Za-z0-9_]*\[`), "arrays (${name[…]})"},
	{regexp.MustCompile(`\$'`), "$'…' quoting"},
}

// ShellWarnings lists bash-only syntax in a job that runs under sh, each
// with the fix. Jobs with shell: bash get none.
func ShellWarnings(j CompiledJob) []string {
	if j.Shell == ShellBash {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, line := range strings.Split(j.Script, "\n") {
		for _, b := range bashisms {
			if !seen[b.what] && b.re.MatchString(line) {
				seen[b.what] = true
				out = append(out, fmt.Sprintf("job %q runs under sh, which has no %s (%q); add shell: bash, or rewrite it in POSIX sh",
					j.Name, b.what, strings.TrimSpace(line)))
			}
		}
	}
	return out
}
