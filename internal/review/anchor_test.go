package review

import "testing"

func TestReanchor(t *testing.T) {
	// Old file lines 1..10; the new head inserts two lines after 2, edits 5
	// (whitespace only) and deletes 8.
	patch := `diff --git a/app.go b/app.go
index 1111111..2222222 100644
--- a/app.go
+++ b/app.go
@@ -1,9 +1,10 @@
 one
 two
+inserted a
+inserted b
 three
 four
-	five()
+five()
 six
 seven
-eight
 nine
`
	for _, c := range []struct {
		line int
		fp   string
		want int
		why  string
	}{
		{1, "", 1, "before the change"},
		{3, "", 5, "shifted by the insertion"},
		{5, "five()", 7, "edited in place, same text"},
		{8, "eight", 0, "deleted: outdated"},
		{9, "", 10, "context after the deletion"},
		{10, "", 11, "past the hunk: shifted by the net +1"},
	} {
		if got := Reanchor(patch, c.line, c.fp); got != c.want {
			t.Errorf("%s: line %d → %d, want %d", c.why, c.line, got, c.want)
		}
	}
	if Reanchor("", 42, "") != 42 {
		t.Error("an unchanged file moved its lines")
	}
	if Reanchor("diff --git a/x b/x\ndeleted file mode 100644\n@@ -1,2 +0,0 @@\n-a\n-b\n", 1, "a") != 0 {
		t.Error("a deleted file kept its thread")
	}
	// Two hunks: offsets accumulate.
	two := "@@ -2,0 +3,2 @@\n+x\n+y\n@@ -20,2 +22,1 @@\n a\n-b\n"
	if got := Reanchor(two, 30, ""); got != 31 {
		t.Errorf("after two hunks: %d, want 31", got)
	}
	if got := Reanchor(two, 10, ""); got != 12 {
		t.Errorf("between hunks: %d, want 12", got)
	}
}

func TestLineText(t *testing.T) {
	p := "diff --git a/x b/x\n--- a/x\n+++ b/x\n@@ -3,3 +3,3 @@\n keep\n-old line\n+new line\n tail\n"
	if got := LineText(p, "new", 4); got != "new line" {
		t.Errorf("new 4 = %q", got)
	}
	if got := LineText(p, "old", 4); got != "old line" {
		t.Errorf("old 4 = %q", got)
	}
	if got := LineText(p, "new", 5); got != "tail" {
		t.Errorf("new 5 = %q", got)
	}
	if got := LineText(p, "new", 99); got != "" {
		t.Errorf("absent line = %q", got)
	}
}
