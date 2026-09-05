package compiler

import (
	"slices"
	"testing"
)

func TestMergeStages(t *testing.T) {
	tests := []struct {
		name    string
		base    []string // included fragments
		overlay []string // main config
		want    []string
	}{
		{
			// The reported bug: the main config already positions `test`, so its
			// order wins instead of the include's stage being hoisted.
			name:    "main config position wins for a stage it declares",
			base:    []string{"test"},
			overlay: []string{"lint", "test", "build", "deploy"},
			want:    []string{"lint", "test", "build", "deploy"},
		},
		{
			// Unchanged behaviour: an include's stage the main config does NOT
			// mention still sorts ahead, so an unpositioned scan runs first.
			name:    "include-only stage still sorts ahead",
			base:    []string{"test"},
			overlay: []string{"build", "deploy"},
			want:    []string{"test", "build", "deploy"},
		},
		{
			name:    "partially positioned",
			base:    []string{"scan", "test"},
			overlay: []string{"lint", "test", "build"},
			want:    []string{"scan", "lint", "test", "build"},
		},
		{
			name:    "main config order preserved exactly when it lists everything",
			base:    []string{"test", "build"},
			overlay: []string{"deploy", "build", "test"},
			want:    []string{"deploy", "build", "test"},
		},
		{name: "empty base", base: nil, overlay: []string{"a", "b"}, want: []string{"a", "b"}},
		{name: "empty overlay", base: []string{"a", "b"}, overlay: nil, want: []string{"a", "b"}},
		{name: "both empty", base: nil, overlay: nil, want: []string{}},
		{
			name:    "duplicates collapse",
			base:    []string{"test", "test"},
			overlay: []string{"lint", "lint", "test"},
			want:    []string{"lint", "test"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mergeStages(tt.base, tt.overlay)
			if !slices.Equal(got, tt.want) {
				t.Errorf("mergeStages(%v, %v) = %v, want %v", tt.base, tt.overlay, got, tt.want)
			}
		})
	}
}

// scanTemplate is a stand-in for a built-in: it declares `stages: [test]` and
// one job in it, exactly as the shipped security/* templates do.
func scanTemplate(name string) (string, bool, error) {
	if name != "scan-tmpl" {
		return "", false, nil
	}
	return `
stages: [test]
jobs:
  scan:
    stage: test
    image: alpine:3
    script: [echo scanning]
    allow_failure: true
`, true, nil
}

// stageOrder reads the compiled stage order back off the jobs, which is what the
// UI renders the chart from.
func stageOrder(jobs []CompiledJob) []string {
	byIdx := map[int]string{}
	for _, j := range jobs {
		byIdx[j.StageIdx] = j.Stage
	}
	idxs := make([]int, 0, len(byIdx))
	for i := range byIdx {
		idxs = append(idxs, i)
	}
	slices.Sort(idxs)
	out := make([]string, 0, len(idxs))
	for _, i := range idxs {
		out = append(out, byIdx[i])
	}
	return out
}

// The reported symptom, end to end through Compile: a config that declares
// `stages: [lint, test, build, deploy]` and includes a template must keep lint
// before test.
func TestIncludeHonoursMainStageOrder(t *testing.T) {
	jobs, err := Compile(`
include: [{template: scan-tmpl}]
stages: [lint, test, build, deploy]
jobs:
  lint:
    stage: lint
    script: [echo linting]
  build:
    stage: build
    script: [echo building]
  deploy:
    stage: deploy
    script: [echo deploying]
`, "main", SourcePush, scanTemplate)
	if err != nil {
		t.Fatal(err)
	}
	got := stageOrder(jobs)
	want := []string{"lint", "test", "build", "deploy"}
	if !slices.Equal(got, want) {
		t.Errorf("stage order = %v, want %v", got, want)
	}
	// And the scan job really is in the positioned stage.
	for _, j := range jobs {
		if j.Name == "scan" && j.Stage != "test" {
			t.Errorf("scan landed in stage %q, want test", j.Stage)
		}
	}
}

// Regression: a main config that does NOT declare the included stage keeps the
// documented security-first behaviour — the scan stage sorts ahead.
func TestIncludeStageSortsAheadWhenUnpositioned(t *testing.T) {
	jobs, err := Compile(`
include: [{template: scan-tmpl}]
stages: [build, deploy]
jobs:
  build:
    stage: build
    script: [echo building]
  deploy:
    stage: deploy
    script: [echo deploying]
`, "main", SourcePush, scanTemplate)
	if err != nil {
		t.Fatal(err)
	}
	got := stageOrder(jobs)
	want := []string{"test", "build", "deploy"}
	if !slices.Equal(got, want) {
		t.Errorf("stage order = %v, want %v (scans run security-first when unpositioned)", got, want)
	}
}

// needs across stages is validated against the merged order, so repositioning a
// stage has to reposition its dependency checks too: a scan in a positioned
// `test` stage can now be depended on by a later `build` job.
func TestNeedsAcrossRepositionedIncludeStage(t *testing.T) {
	jobs, err := Compile(`
include: [{template: scan-tmpl}]
stages: [lint, test, build]
jobs:
  lint:
    stage: lint
    script: [echo linting]
  build:
    stage: build
    needs: [scan]
    script: [echo building]
`, "main", SourcePush, scanTemplate)
	if err != nil {
		t.Fatalf("build should be allowed to need scan in the earlier test stage: %v", err)
	}
	for _, j := range jobs {
		if j.Name == "build" && !slices.Contains(j.Needs, "scan") {
			t.Errorf("build needs = %v, want it to include scan", j.Needs)
		}
	}
}

// The inverse must still be rejected: a job cannot need one in a later stage.
func TestNeedsIntoLaterStageStillRejected(t *testing.T) {
	_, err := Compile(`
include: [{template: scan-tmpl}]
stages: [lint, test]
jobs:
  lint:
    stage: lint
    needs: [scan]
    script: [echo linting]
`, "main", SourcePush, scanTemplate)
	if err == nil {
		t.Fatal("expected an error: lint (stage lint) cannot need scan (stage test)")
	}
}
