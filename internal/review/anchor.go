package review

import (
	"regexp"
	"strconv"
	"strings"
)

// Re-anchoring: when a change gets a new head, a thread left on line L of the
// old head's file is moved to where that line is now, using the diff between
// the two heads. Hosts with native threads (GitHub) do this themselves; for
// the rest Forge does it, so "outdated" means the same thing everywhere.

var hunkRe = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

// Reanchor maps line (1-based, on the new side of the file at the old head)
// through patch, the file's diff from the old head to the new head. An empty
// patch means the file did not change. It returns the line's new number, or
// 0 when the line is gone (the thread is outdated). fingerprint, the line's
// text when the thread was left, lets a line that was edited in place but
// still reads the same (whitespace moves, a neighbour changed) keep its
// thread.
func Reanchor(patch string, line int, fingerprint string) int {
	if strings.TrimSpace(patch) == "" {
		return line
	}
	delta := 0
	lines := strings.Split(patch, "\n")
	for i := 0; i < len(lines); i++ {
		m := hunkRe.FindStringSubmatch(lines[i])
		if m == nil {
			if strings.HasPrefix(lines[i], "deleted file mode") {
				return 0
			}
			continue
		}
		oldStart, _ := strconv.Atoi(m[1])
		oldLen := 1
		if m[2] != "" {
			oldLen, _ = strconv.Atoi(m[2])
		}
		newStart, _ := strconv.Atoi(m[3])
		if oldLen == 0 {
			oldStart++ // a pure insertion after line oldStart
		}
		if line < oldStart {
			return line + delta
		}
		// Walk this hunk.
		o, n := oldStart, newStart
		var gone bool
		var removed string
		for j := i + 1; j < len(lines) && !hunkRe.MatchString(lines[j]); j++ {
			l := lines[j]
			switch {
			case strings.HasPrefix(l, " ") || l == "":
				if o == line {
					return n
				}
				o++
				n++
			case strings.HasPrefix(l, "-"):
				if o == line {
					gone, removed = true, l[1:]
				}
				o++
			case strings.HasPrefix(l, "+"):
				if gone {
					text := l[1:]
					if fingerprint != "" && strings.TrimSpace(text) == strings.TrimSpace(fingerprint) {
						return n
					}
					if strings.TrimSpace(text) == strings.TrimSpace(removed) {
						return n
					}
				}
				n++
			}
		}
		if gone || (line >= oldStart && line < oldStart+oldLen) {
			return 0
		}
		delta = n - o
	}
	return line + delta
}

// LineText returns the text of line on side ("new" or "old") of a file's
// patch, or "" when the patch does not show that line.
func LineText(patch, side string, line int) string {
	o, n := 0, 0
	for _, l := range strings.Split(patch, "\n") {
		if m := hunkRe.FindStringSubmatch(l); m != nil {
			o, _ = strconv.Atoi(m[1])
			n, _ = strconv.Atoi(m[3])
			continue
		}
		if o == 0 && n == 0 {
			continue // file headers
		}
		switch {
		case strings.HasPrefix(l, "+"):
			if side == "new" && n == line {
				return l[1:]
			}
			n++
		case strings.HasPrefix(l, "-"):
			if side == "old" && o == line {
				return l[1:]
			}
			o++
		case strings.HasPrefix(l, " "):
			if (side == "new" && n == line) || (side == "old" && o == line) {
				return l[1:]
			}
			o++
			n++
		}
	}
	return ""
}
