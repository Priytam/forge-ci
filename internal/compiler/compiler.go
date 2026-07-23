// Package compiler turns pipeline YAML into a validated job DAG.
//
// DSL:
//
//	stages: [build, test, deploy]
//	jobs:
//	  build-app:
//	    stage: build
//	    image: alpine:3          # optional; used by the docker executor
//	    script: [echo hi]        # required
//	    needs: [other-job]       # optional; must reference an EARLIER stage.
//	                             # default: all jobs of the previous stage
//	    environment: production  # optional; protected envs require approval
//	    variables: {KEY: value}  # optional env vars
//	    only: [main, release-*]  # optional; include job only for matching refs
//	    except: [main]           # optional; exclude job for matching refs
//
// only/except patterns are shell globs matched against the pipeline ref, so
// the same YAML compiles to different DAGs for different refs (dev vs prod).
//
// Cycles are impossible by construction: an explicit need must live in an
// earlier stage, and implicit needs only point to earlier stages.
package compiler

import (
	"fmt"
	"path"
	"sort"
	"time"

	"gopkg.in/yaml.v3"
)

type jobSpec struct {
	Stage       string            `yaml:"stage"`
	Image       string            `yaml:"image"`
	Script      []string          `yaml:"script"`
	Needs       []string          `yaml:"needs"`
	Environment string            `yaml:"environment"`
	Variables   map[string]string `yaml:"variables"`
	Only        []string          `yaml:"only"`
	Except      []string          `yaml:"except"`
	Tags        []string          `yaml:"tags"`
	Artifacts   artifactSpec      `yaml:"artifacts"`
	Timeout     string            `yaml:"timeout"` // Go duration, e.g. "30m", "2h"
}

type artifactSpec struct {
	Paths []string `yaml:"paths"`
}

func refMatches(patterns []string, ref string) bool {
	for _, p := range patterns {
		if p == ref {
			return true
		}
		if ok, err := path.Match(p, ref); err == nil && ok {
			return true
		}
	}
	return false
}

func (s jobSpec) includedFor(ref string) bool {
	if len(s.Only) > 0 && !refMatches(s.Only, ref) {
		return false
	}
	if len(s.Except) > 0 && refMatches(s.Except, ref) {
		return false
	}
	return true
}

type config struct {
	Stages  []string           `yaml:"stages"`
	Jobs    map[string]jobSpec `yaml:"jobs"`
	Default struct {
		Timeout string `yaml:"timeout"` // pipeline-wide TTL for jobs without their own
	} `yaml:"default"`
}

func parseTimeout(where, v string) (int, error) {
	if v == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < time.Minute {
		return 0, fmt.Errorf("%s: timeout must be a duration of at least 1m (e.g. \"30m\", \"2h\")", where)
	}
	return int(d.Seconds()), nil
}

type CompiledJob struct {
	Name          string
	Stage         string
	StageIdx      int
	Image         string
	Script        string // newline-joined
	Env           map[string]string
	Environment   string
	Needs         []string // job names in earlier stages
	Tags          []string // runner routing: job runs only on runners with all these tags
	ArtifactPaths []string // workspace paths archived after success
	TimeoutSec    int      // 0 = server default
}

// Compile builds the job DAG for one specific ref: jobs whose only/except
// rules exclude the ref are dropped, so dev and prod refs can yield
// different DAGs from the same YAML.
func Compile(yml, ref string) ([]CompiledJob, error) {
	var cfg config
	if err := yaml.Unmarshal([]byte(yml), &cfg); err != nil {
		return nil, fmt.Errorf("invalid YAML: %w", err)
	}
	if len(cfg.Stages) == 0 {
		return nil, fmt.Errorf("config must declare at least one stage")
	}
	if len(cfg.Jobs) == 0 {
		return nil, fmt.Errorf("config must declare at least one job")
	}

	stageIdx := map[string]int{}
	for i, s := range cfg.Stages {
		if _, dup := stageIdx[s]; dup {
			return nil, fmt.Errorf("duplicate stage %q", s)
		}
		stageIdx[s] = i
	}

	// Deterministic order: by stage, then name. Excluded jobs are validated
	// (stage must exist) but dropped from the DAG.
	names := make([]string, 0, len(cfg.Jobs))
	for n, spec := range cfg.Jobs {
		if _, ok := stageIdx[spec.Stage]; !ok {
			return nil, fmt.Errorf("job %q: unknown stage %q", n, spec.Stage)
		}
		if spec.includedFor(ref) {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("no jobs match ref %q (check only/except rules)", ref)
	}
	sort.Slice(names, func(i, j int) bool {
		si, sj := stageIdx[cfg.Jobs[names[i]].Stage], stageIdx[cfg.Jobs[names[j]].Stage]
		if si != sj {
			return si < sj
		}
		return names[i] < names[j]
	})

	included := map[string]bool{}
	jobsByStage := map[int][]string{}
	for _, n := range names {
		included[n] = true
		jobsByStage[stageIdx[cfg.Jobs[n].Stage]] = append(jobsByStage[stageIdx[cfg.Jobs[n].Stage]], n)
	}

	var out []CompiledJob
	for _, n := range names {
		spec := cfg.Jobs[n]
		idx := stageIdx[spec.Stage]
		if len(spec.Script) == 0 {
			return nil, fmt.Errorf("job %q: script is required", n)
		}
		// TTL precedence: job timeout > YAML default.timeout > server default.
		timeoutSec, err := parseTimeout(fmt.Sprintf("job %q", n), spec.Timeout)
		if err != nil {
			return nil, err
		}
		if timeoutSec == 0 {
			if timeoutSec, err = parseTimeout("default", cfg.Default.Timeout); err != nil {
				return nil, err
			}
		}

		needs := spec.Needs
		if needs == nil {
			// Implicit needs: all included jobs of the nearest earlier
			// non-empty stage (a stage can be empty for this ref).
			for prev := idx - 1; prev >= 0; prev-- {
				if len(jobsByStage[prev]) > 0 {
					needs = append([]string(nil), jobsByStage[prev]...)
					break
				}
			}
		}
		for _, dep := range needs {
			depSpec, ok := cfg.Jobs[dep]
			if !ok {
				return nil, fmt.Errorf("job %q: needs unknown job %q", n, dep)
			}
			if !included[dep] {
				return nil, fmt.Errorf("job %q: needs %q which is excluded for ref %q", n, dep, ref)
			}
			if stageIdx[depSpec.Stage] >= idx {
				return nil, fmt.Errorf("job %q: needs %q which is not in an earlier stage", n, dep)
			}
		}

		script := ""
		for i, line := range spec.Script {
			if i > 0 {
				script += "\n"
			}
			script += line
		}
		env := spec.Variables
		if env == nil {
			env = map[string]string{}
		}
		out = append(out, CompiledJob{
			Name:          n,
			Stage:         spec.Stage,
			StageIdx:      idx,
			Image:         spec.Image,
			Script:        script,
			Env:           env,
			Environment:   spec.Environment,
			Needs:         needs,
			Tags:          spec.Tags,
			ArtifactPaths: spec.Artifacts.Paths,
			TimeoutSec:    timeoutSec,
		})
	}
	return out, nil
}
