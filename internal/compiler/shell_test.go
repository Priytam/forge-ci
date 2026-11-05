package compiler

import (
	"strings"
	"testing"
)

func TestShellResolution(t *testing.T) {
	yml := `
default:
  shell: bash
stages: [b]
jobs:
  inherits:
    stage: b
    script:
      - 'echo "${CI_COMMIT_SHA:0:7}"'
  posix:
    stage: b
    shell: sh
    script: [echo hi]
`
	jobs, err := Compile(yml, "main", SourceAPI, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, j := range jobs {
		got[j.Name] = j.Shell
	}
	if got["inherits"] != "bash" || got["posix"] != "sh" {
		t.Fatalf("shells = %v", got)
	}

	jobs, err = Compile("stages: [b]\njobs:\n  a:\n    stage: b\n    script: [true]\n", "main", SourceAPI, nil)
	if err != nil || jobs[0].Shell != "sh" {
		t.Fatalf("default shell = %q (%v), want sh", jobs[0].Shell, err)
	}
	if _, err := Compile("stages: [b]\njobs:\n  a:\n    stage: b\n    shell: zsh\n    script: [true]\n", "main", SourceAPI, nil); err == nil ||
		!strings.Contains(err.Error(), "shell must be sh or bash") {
		t.Fatalf("shell: zsh accepted: %v", err)
	}
}

func TestShellWarnings(t *testing.T) {
	for _, c := range []struct {
		script string
		want   string // "" = no warning
	}{
		{`echo "${CI_COMMIT_SHA:0:7}"`, "substring"},
		{`echo ${TAG: -3}`, "substring"},
		{`echo ${NAME:-default}`, ""},
		{`echo ${NAME:=x} ${#NAME} ${NAME%.*}`, ""},
		{`echo ${REF//\//-}`, "pattern/replacement"},
		{`echo ${ENV^^}`, "case change"},
		{`if [[ -n "$X" ]]; then echo; fi`, "[[ … ]]"},
		{`[ -n "$X" ] && echo`, ""},
		{`diff <(sort a) <(sort b)`, "process substitution"},
		{`ARR=(a b c)`, "arrays"},
		{`echo $(cat x)`, ""},
		{`printf $'a\tb'`, "quoting"},
	} {
		w := ShellWarnings(CompiledJob{Name: "j", Shell: "sh", Script: c.script})
		switch {
		case c.want == "" && len(w) > 0:
			t.Errorf("%s: unexpected warning %v", c.script, w)
		case c.want != "" && (len(w) != 1 || !strings.Contains(w[0], c.want)):
			t.Errorf("%s: warnings %v, want one about %q", c.script, w, c.want)
		}
	}
	if w := ShellWarnings(CompiledJob{Name: "j", Shell: "bash", Script: `echo ${X:0:7}`}); len(w) != 0 {
		t.Errorf("bash job warned: %v", w)
	}
}
