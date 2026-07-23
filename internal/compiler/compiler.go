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
//
// Cycles are impossible by construction: an explicit need must live in an
// earlier stage, and implicit needs only point one stage back.
package compiler

import (
	"fmt"
	"sort"

	"gopkg.in/yaml.v3"
)

type jobSpec struct {
	Stage       string            `yaml:"stage"`
	Image       string            `yaml:"image"`
	Script      []string          `yaml:"script"`
	Needs       []string          `yaml:"needs"`
	Environment string            `yaml:"environment"`
	Variables   map[string]string `yaml:"variables"`
}

type config struct {
	Stages []string           `yaml:"stages"`
	Jobs   map[string]jobSpec `yaml:"jobs"`
}

type CompiledJob struct {
	Name        string
	Stage       string
	StageIdx    int
	Image       string
	Script      string // newline-joined
	Env         map[string]string
	Environment string
	Needs       []string // job names in earlier stages
}

func Compile(yml string) ([]CompiledJob, error) {
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

	// Deterministic order: by stage, then name.
	names := make([]string, 0, len(cfg.Jobs))
	for n := range cfg.Jobs {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		si, sj := stageIdx[cfg.Jobs[names[i]].Stage], stageIdx[cfg.Jobs[names[j]].Stage]
		if si != sj {
			return si < sj
		}
		return names[i] < names[j]
	})

	jobsByStage := map[int][]string{}
	for _, n := range names {
		spec := cfg.Jobs[n]
		idx, ok := stageIdx[spec.Stage]
		if !ok {
			return nil, fmt.Errorf("job %q: unknown stage %q", n, spec.Stage)
		}
		jobsByStage[idx] = append(jobsByStage[idx], n)
	}

	var out []CompiledJob
	for _, n := range names {
		spec := cfg.Jobs[n]
		idx := stageIdx[spec.Stage]
		if len(spec.Script) == 0 {
			return nil, fmt.Errorf("job %q: script is required", n)
		}

		needs := spec.Needs
		if needs == nil && idx > 0 {
			needs = append([]string(nil), jobsByStage[idx-1]...)
		}
		for _, dep := range needs {
			depSpec, ok := cfg.Jobs[dep]
			if !ok {
				return nil, fmt.Errorf("job %q: needs unknown job %q", n, dep)
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
			Name:        n,
			Stage:       spec.Stage,
			StageIdx:    idx,
			Image:       spec.Image,
			Script:      script,
			Env:         env,
			Environment: spec.Environment,
			Needs:       needs,
		})
	}
	return out, nil
}
