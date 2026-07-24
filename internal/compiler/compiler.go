// Package compiler turns pipeline YAML into a validated job DAG.
//
// Core DSL:
//
//	stages: [build, test, deploy]
//	jobs:
//	  build-app:
//	    stage: build
//	    image: alpine:3          # optional; used by the docker executor
//	    script: [echo hi]        # required (may be inherited via extends)
//	    needs: [other-job]       # optional; must reference an EARLIER stage.
//	                             # default: all jobs of the previous stage
//	    environment: production  # optional; protected envs require approval
//	    variables: {KEY: value}  # optional env vars
//	    only: [main, release-*]  # optional; include job only for matching refs
//	    except: [main]           # optional; exclude job for matching refs
//	    retry: 2                 # optional; retry on failure up to N times (0..10)
//	    artifacts:               # optional; archived after a successful job
//	      paths: [dist/]         #   workspace paths tar'd + uploaded
//	      expire_in: 7d          #   optional per-job artifact TTL (see below)
//	      reports:               #   optional test reports parsed server-side
//	        junit: [report.xml]  #     JUnit XML glob(s) -> pass/fail summary
//
// artifacts.expire_in accepts Go durations plus d/w suffixes: "30m", "24h",
// "7d", "2w" (and compound Go forms like "1h30m"). An artifact with an
// expire_in is deleted (blob + row) once it elapses, independent of
// RETENTION_DAYS; no expire_in falls back to the RETENTION_DAYS backstop.
//
// Top-level keys:
//
//	auto_cancel: true            # optional (default true); cancel older
//	                             # non-terminal pipelines for the same repo+ref
//	fail_fast: true              # optional (default false); on the first genuine
//	                             # job failure, cancel the pipeline's other
//	                             # in-flight and not-yet-started jobs
//	default: {timeout: 30m, retry: 1}
//	include: [{template: name}]  # compose from registered per-repo templates
//
// GitLab-style authoring extensions (see docs/pipeline-dsl.md):
//
//   - rules:    per-job ordered list; first match decides inclusion + when.
//     Supersedes only/except for any job that declares it.
//   - include:  merge reusable fragments (registered templates) into the config.
//   - extends:  inherit from one or more base jobs (hidden ".name" templates
//     are not emitted); deep-merged, child overrides parent.
//   - parallel: expand a job into N instances, or a matrix into one job per
//     variable combination.
//
// only/except patterns are shell globs matched against the pipeline ref, so the
// same YAML compiles to different DAGs for different refs (dev vs prod).
//
// Cycles are impossible by construction: an explicit need must live in an
// earlier stage, and implicit needs only point to earlier stages.
package compiler

import (
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

// Pipeline source values threaded into the rules if: context as
// CI_PIPELINE_SOURCE.
const (
	SourceAPI          = "api"
	SourceWebhook      = "webhook"
	SourcePush         = "push"
	SourceSchedule     = "schedule"
	SourceMergeRequest = "merge_request" // GitHub pull_request / Bitbucket pull-request
)

const (
	maxRetry    = 10
	maxParallel = 200 // upper bound on parallel:N / matrix expansion
)

type jobSpec struct {
	Stage        string            `yaml:"stage"`
	Image        string            `yaml:"image"`
	Script       []string          `yaml:"script"`
	Needs        []string          `yaml:"needs"`
	Environment  string            `yaml:"environment"`
	Variables    map[string]string `yaml:"variables"`
	Only         []string          `yaml:"only"`
	Except       []string          `yaml:"except"`
	Tags         []string          `yaml:"tags"`
	Artifacts    artifactSpec      `yaml:"artifacts"`
	Cache        cacheSpec         `yaml:"cache"`
	Services     []serviceSpec     `yaml:"services"`
	Timeout      string            `yaml:"timeout"` // Go duration, e.g. "30m", "2h"
	Retry        *int              `yaml:"retry"`   // 0..10; nil = inherit default
	Rules        []ruleSpec        `yaml:"rules"`
	Extends      stringOrSlice     `yaml:"extends"`
	Parallel     *parallelSpec     `yaml:"parallel"`
	When         string            `yaml:"when"`          // on_success|manual|never|always (job-level; rules override)
	AllowFailure *bool             `yaml:"allow_failure"` // failure does not fail dependents/pipeline
}

type artifactSpec struct {
	Paths    []string    `yaml:"paths"`
	ExpireIn string      `yaml:"expire_in"` // per-job artifact TTL; Go duration + d/w suffixes
	Reports  reportsSpec `yaml:"reports"`
}

// reportsSpec models artifacts.reports: test reports collected by the runner
// and parsed by the server into per-job summaries. Only JUnit is supported.
type reportsSpec struct {
	JUnit stringOrSlice `yaml:"junit"` // one or more workspace globs of JUnit XML
}

// cacheSpec models a per-job cache: block (GitLab-style):
//
//	cache:
//	  key: v1-deps                # literal key, OR:
//	  key: { files: [go.sum], prefix: v1 }  # content-addressed by file hashes
//	  paths: [vendor/, .cache/]   # workspace globs restored/saved
//	  policy: pull-push           # pull-push (default) | pull | push
//
// Cache is opt-in: a job with no paths declares no cache. Unlike artifacts
// (per-job, per-pipeline), cache is shared ACROSS pipelines for the same
// repo+key, so a dependency cache warmed by one run speeds up the next.
type cacheSpec struct {
	Key    cacheKey `yaml:"key"`
	Paths  []string `yaml:"paths"`
	Policy string   `yaml:"policy"` // pull-push (default) | pull | push
}

// cacheKey accepts either a scalar literal key or a mapping with content-
// addressed hashing:
//
//	key: my-literal-key
//	key: { files: [go.sum, go.mod], prefix: v1 }
//
// In the mapping form the runner hashes the listed files' contents (available
// after checkout) and appends the hash to the optional prefix, so a changed
// lockfile misses cleanly and an unchanged one hits.
type cacheKey struct {
	Literal string   // scalar key, or the prefix of a files: key
	Files   []string // files whose contents are hashed into the key (runner-side)
}

func (k *cacheKey) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		return n.Decode(&k.Literal)
	case yaml.MappingNode:
		var raw struct {
			Files  []string `yaml:"files"`
			Prefix string   `yaml:"prefix"`
		}
		if err := n.Decode(&raw); err != nil {
			return err
		}
		if len(raw.Files) == 0 {
			return fmt.Errorf("cache.key.files must be a non-empty list of files")
		}
		k.Files = raw.Files
		k.Literal = raw.Prefix
		return nil
	default:
		return fmt.Errorf("cache.key must be a string or a {files: [...]} mapping")
	}
}

// cachePolicies are the accepted policy values; "" defaults to pull-push.
var cachePolicies = map[string]bool{"pull": true, "push": true, "pull-push": true}

// validateCache checks a job's cache block and returns the resolved policy.
// A block with no paths means "no cache" and is accepted (returns "").
func validateCache(where string, c cacheSpec) (string, error) {
	if len(c.Paths) == 0 {
		if c.Key.Literal != "" || len(c.Key.Files) > 0 || c.Policy != "" {
			return "", fmt.Errorf("%s: cache declares a key/policy but no paths", where)
		}
		return "", nil
	}
	if c.Key.Literal == "" && len(c.Key.Files) == 0 {
		return "", fmt.Errorf("%s: cache requires a key (a literal string or {files: [...]})", where)
	}
	policy := c.Policy
	if policy == "" {
		policy = "pull-push"
	}
	if !cachePolicies[policy] {
		return "", fmt.Errorf("%s: cache.policy must be pull, push or pull-push", where)
	}
	return policy, nil
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

func (s jobSpec) includedByOnlyExcept(ref string) bool {
	if len(s.Only) > 0 && !refMatches(s.Only, ref) {
		return false
	}
	if len(s.Except) > 0 && refMatches(s.Except, ref) {
		return false
	}
	return true
}

type config struct {
	Stages     []string           `yaml:"stages"`
	Jobs       map[string]jobSpec `yaml:"jobs"`
	AutoCancel *bool              `yaml:"auto_cancel"` // nil = default true
	FailFast   *bool              `yaml:"fail_fast"`   // nil = default false
	Include    []includeSpec      `yaml:"include"`
	Default    struct {
		Timeout string `yaml:"timeout"` // pipeline-wide TTL for jobs without their own
		Retry   *int   `yaml:"retry"`   // pipeline-wide retry for jobs without their own
	} `yaml:"default"`
}

func validateRetry(where string, v *int) (int, error) {
	if v == nil {
		return 0, nil
	}
	if *v < 0 || *v > maxRetry {
		return 0, fmt.Errorf("%s: retry must be between 0 and %d", where, maxRetry)
	}
	return *v, nil
}

// PipelineOptions carries pipeline-level (non-job) settings parsed from the
// YAML that the store needs at creation time.
type PipelineOptions struct {
	AutoCancel bool // cancel older non-terminal pipelines for the same repo+ref
	FailFast   bool // on the first genuine job failure, cancel the pipeline's other jobs
}

// Options parses just the pipeline-level settings from the YAML. AutoCancel
// defaults to true and FailFast defaults to false when the key is absent. Both
// are read from the main config only (not from included templates).
func Options(yml string) (PipelineOptions, error) {
	var cfg config
	if err := yaml.Unmarshal([]byte(yml), &cfg); err != nil {
		return PipelineOptions{}, fmt.Errorf("invalid YAML: %w", err)
	}
	autoCancel := true
	if cfg.AutoCancel != nil {
		autoCancel = *cfg.AutoCancel
	}
	failFast := false
	if cfg.FailFast != nil {
		failFast = *cfg.FailFast
	}
	return PipelineOptions{AutoCancel: autoCancel, FailFast: failFast}, nil
}

// parseExpireIn parses artifacts.expire_in into whole seconds. It accepts any
// Go duration (e.g. "30m", "24h", "1h30m") plus the day/week suffixes GitLab
// authors expect ("7d", "2w") which time.ParseDuration does not understand. An
// empty value yields 0 (no explicit expiry — falls back to RETENTION_DAYS).
func parseExpireIn(where, v string) (int, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, nil
	}
	if n := len(v); n >= 2 {
		switch v[n-1] {
		case 'd', 'w':
			num, err := strconv.ParseFloat(v[:n-1], 64)
			if err != nil || num < 0 {
				return 0, fmt.Errorf("%s: invalid expire_in %q (want e.g. 30m, 24h, 7d, 2w)", where, v)
			}
			hours := 24.0
			if v[n-1] == 'w' {
				hours = 24.0 * 7.0
			}
			return int(num * hours * 3600), nil
		}
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("%s: invalid expire_in %q (want e.g. 30m, 24h, 7d, 2w)", where, v)
	}
	return int(d.Seconds()), nil
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
	// ArtifactExpireSeconds is the per-job artifact TTL in seconds (0 = none;
	// falls back to RETENTION_DAYS). The server stamps artifacts.expires_at =
	// now()+this at upload time.
	ArtifactExpireSeconds int
	// ReportJUnit are workspace globs of JUnit XML the runner collects after a
	// successful job; the server parses them into a per-job test summary.
	ReportJUnit []string
	// Cache (opt-in). CachePaths empty means the job declares no cache.
	CachePaths    []string // workspace paths restored before / saved after the script
	CacheKey      string   // literal key, or the prefix when CacheKeyFiles is set
	CacheKeyFiles []string // files whose contents the runner hashes into the key
	CachePolicy   string   // pull | push | pull-push (empty when no cache)
	TimeoutSec    int      // 0 = server default
	Retry         int      // additional attempts on failure (0..10); 0 = no retry
	Manual        bool     // when: manual — starts gated, released by a manual "play"
	AllowFailure  bool     // failure does not block dependents or fail the pipeline
	// Services are sidecar containers started alongside the job (docker/k8s only).
	Services []proto.ServiceSpec
}

// buildContext assembles the base variable context for rules if: expressions.
// Forge cannot distinguish a tag ref from a branch ref at compile time (a
// webhook carries only the ref string), so CI_COMMIT_BRANCH is set to the ref
// and CI_COMMIT_TAG is left undefined — documented in docs/pipeline-dsl.md.
//
// extra carries source-specific context (e.g. the CI_MERGE_REQUEST_* vars for a
// merge_request pipeline). It is overlaid last so a caller can set/override any
// key, and its entries are also injected into every emitted job's Env by Compile
// so job scripts can read them.
func buildContext(ref, source string, extra map[string]string) map[string]string {
	ctx := map[string]string{
		"CI_COMMIT_REF":      ref,
		"CI_COMMIT_REF_NAME": ref,
		"CI_COMMIT_BRANCH":   ref,
		"CI_PIPELINE_SOURCE": source,
	}
	for k, v := range extra {
		ctx[k] = v
	}
	return ctx
}

// Compile builds the job DAG for one specific ref and pipeline source.
//
// The pipeline goes through: include resolution -> extends resolution -> hidden
// (template) job removal -> per-job inclusion (rules, else only/except) ->
// parallel/matrix expansion -> needs wiring (with matrix fan-in) -> validation.
//
// source is threaded into the rules if: context as CI_PIPELINE_SOURCE. tmpl
// resolves include: templates; it may be nil when the caller has no template
// store (any include then errors).
//
// extra is optional source-specific context (only the first map is used). For a
// merge_request pipeline the webhook handler passes the CI_MERGE_REQUEST_* vars
// here; they become visible to rules if: expressions AND are injected into every
// emitted job's Env so job scripts can read them. Existing callers pass no extra
// map and are unaffected.
func Compile(yml, ref, source string, tmpl TemplateFunc, extra ...map[string]string) ([]CompiledJob, error) {
	var extraCtx map[string]string
	if len(extra) > 0 {
		extraCtx = extra[0]
	}
	var cfg config
	if err := yaml.Unmarshal([]byte(yml), &cfg); err != nil {
		return nil, fmt.Errorf("invalid YAML: %w", err)
	}

	// 1. Compose included templates into the config.
	cfg, err := resolveIncludes(cfg, tmpl)
	if err != nil {
		return nil, err
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

	// 2. Resolve extends: chains into fully-merged specs.
	resolved, err := resolveExtends(cfg.Jobs)
	if err != nil {
		return nil, err
	}

	// 3. Partition into emittable (real) jobs and hidden templates. Hidden jobs
	// (leading dot) are never emitted; they exist only to be extended.
	baseCtx := buildContext(ref, source, extraCtx)

	type decision struct {
		spec         jobSpec
		included     bool
		manual       bool
		allowFailure bool
		extraVars    map[string]string
	}
	decisions := map[string]decision{}
	names := make([]string, 0, len(resolved))

	for name := range resolved {
		if strings.HasPrefix(name, ".") {
			continue // hidden template job — not emitted
		}
		spec := resolved[name]
		if _, ok := stageIdx[spec.Stage]; !ok {
			return nil, fmt.Errorf("job %q: unknown stage %q", name, spec.Stage)
		}
		names = append(names, name)

		// Per-job rules context: base CI vars overlaid with the job's variables.
		ctx := map[string]string{}
		for k, v := range baseCtx {
			ctx[k] = v
		}
		for k, v := range spec.Variables {
			ctx[k] = v
		}

		var d decision
		d.spec = spec
		if len(spec.Rules) > 0 {
			// rules: supersede only/except entirely for this job.
			outcome, err := evalRules(spec.Rules, ctx)
			if err != nil {
				return nil, fmt.Errorf("job %q: %w", name, err)
			}
			d.included = outcome.included
			d.manual = outcome.when == "manual"
			d.extraVars = outcome.variables
			d.allowFailure = spec.AllowFailure != nil && *spec.AllowFailure
			if outcome.allowFailure != nil {
				d.allowFailure = *outcome.allowFailure
			}
		} else {
			// Legacy path: only/except + optional job-level when.
			if spec.When != "" && !validWhen[spec.When] {
				return nil, fmt.Errorf("job %q: invalid when %q (want on_success|manual|always|never)", name, spec.When)
			}
			included := spec.includedByOnlyExcept(ref)
			if spec.When == "never" {
				included = false
			}
			d.included = included
			d.manual = spec.When == "manual"
			d.allowFailure = spec.AllowFailure != nil && *spec.AllowFailure
		}
		decisions[name] = d
	}

	includedOrig := map[string]bool{}
	for _, n := range names {
		if decisions[n].included {
			includedOrig[n] = true
		}
	}

	// 4. Expand parallel/matrix. Each original job yields >=1 instance.
	type instance struct {
		name         string // full instance name (unique)
		orig         string
		spec         jobSpec
		stageIdx     int
		manual       bool
		allowFailure bool
		env          map[string]string
	}
	var instances []instance
	nameToInstances := map[string][]string{}

	// deterministic order over original jobs
	sortedNames := append([]string(nil), names...)
	sort.Slice(sortedNames, func(i, j int) bool {
		a, b := sortedNames[i], sortedNames[j]
		si, sj := stageIdx[decisions[a].spec.Stage], stageIdx[decisions[b].spec.Stage]
		if si != sj {
			return si < sj
		}
		return a < b
	})

	for _, n := range sortedNames {
		if !includedOrig[n] {
			continue
		}
		d := decisions[n]
		exps, err := d.spec.Parallel.expand()
		if err != nil {
			return nil, fmt.Errorf("job %q: %w", n, err)
		}
		for _, e := range exps {
			env := map[string]string{}
			for k, v := range extraCtx { // source context (e.g. CI_MERGE_REQUEST_*); scripts can read these
				env[k] = v
			}
			for k, v := range d.spec.Variables {
				env[k] = v
			}
			for k, v := range d.extraVars { // rule-injected variables
				env[k] = v
			}
			for k, v := range e.vars { // parallel/matrix variables win
				env[k] = v
			}
			inst := instance{
				name:         n + e.nameSuffix,
				orig:         n,
				spec:         d.spec,
				stageIdx:     stageIdx[d.spec.Stage],
				manual:       d.manual,
				allowFailure: d.allowFailure,
				env:          env,
			}
			instances = append(instances, inst)
			nameToInstances[n] = append(nameToInstances[n], inst.name)
		}
	}

	if len(instances) == 0 {
		return nil, fmt.Errorf("no jobs match ref %q (check rules / only / except)", ref)
	}

	// Stable output order: by stage, then instance name.
	sort.Slice(instances, func(i, j int) bool {
		if instances[i].stageIdx != instances[j].stageIdx {
			return instances[i].stageIdx < instances[j].stageIdx
		}
		return instances[i].name < instances[j].name
	})

	stageInstances := map[int][]string{}
	for _, inst := range instances {
		stageInstances[inst.stageIdx] = append(stageInstances[inst.stageIdx], inst.name)
	}

	// 5. Build the DAG.
	var out []CompiledJob
	for _, inst := range instances {
		spec := inst.spec
		if len(spec.Script) == 0 {
			return nil, fmt.Errorf("job %q: script is required", inst.orig)
		}

		// TTL precedence: job timeout > YAML default.timeout > server default.
		timeoutSec, err := parseTimeout(fmt.Sprintf("job %q", inst.orig), spec.Timeout)
		if err != nil {
			return nil, err
		}
		if timeoutSec == 0 {
			if timeoutSec, err = parseTimeout("default", cfg.Default.Timeout); err != nil {
				return nil, err
			}
		}

		// Cache: validate the block and resolve the policy (default pull-push).
		cachePolicy, err := validateCache(fmt.Sprintf("job %q", inst.orig), spec.Cache)
		if err != nil {
			return nil, err
		}

		// Services: resolve aliases and validate (count cap, image required).
		services, err := validateServices(fmt.Sprintf("job %q", inst.orig), spec.Services)
		if err != nil {
			return nil, err
		}

		// Artifacts: per-job expiry (0 = none). reports.junit is carried as-is.
		expireSec, err := parseExpireIn(fmt.Sprintf("job %q", inst.orig), spec.Artifacts.ExpireIn)
		if err != nil {
			return nil, err
		}

		// Retry precedence: job retry > YAML default.retry > 0 (no retry).
		retry, err := validateRetry(fmt.Sprintf("job %q", inst.orig), spec.Retry)
		if err != nil {
			return nil, err
		}
		if spec.Retry == nil {
			if retry, err = validateRetry("default", cfg.Default.Retry); err != nil {
				return nil, err
			}
		}

		// Needs: explicit (with matrix fan-in) or implicit (nearest earlier
		// non-empty stage).
		var needs []string
		if spec.Needs == nil {
			for prev := inst.stageIdx - 1; prev >= 0; prev-- {
				if len(stageInstances[prev]) > 0 {
					needs = append([]string(nil), stageInstances[prev]...)
					break
				}
			}
		} else {
			for _, dep := range spec.Needs {
				// The dependency must be a real (non-hidden) job that exists.
				if _, ok := resolved[dep]; !ok {
					return nil, fmt.Errorf("job %q: needs unknown job %q", inst.orig, dep)
				}
				if strings.HasPrefix(dep, ".") {
					return nil, fmt.Errorf("job %q: needs %q which is a hidden template job", inst.orig, dep)
				}
				if !includedOrig[dep] {
					return nil, fmt.Errorf("job %q: needs %q which is excluded for ref %q", inst.orig, dep, ref)
				}
				if stageIdx[resolved[dep].Stage] >= inst.stageIdx {
					return nil, fmt.Errorf("job %q: needs %q which is not in an earlier stage", inst.orig, dep)
				}
				// Fan-in: depend on every instance of the (possibly matrixed) dep.
				needs = append(needs, nameToInstances[dep]...)
			}
		}

		out = append(out, CompiledJob{
			Name:          inst.name,
			Stage:         spec.Stage,
			StageIdx:      inst.stageIdx,
			Image:         spec.Image,
			Script:        strings.Join(spec.Script, "\n"),
			Env:           inst.env,
			Environment:   spec.Environment,
			Needs:         needs,
			Tags:          spec.Tags,
			ArtifactPaths: spec.Artifacts.Paths,

			ArtifactExpireSeconds: expireSec,
			ReportJUnit:           spec.Artifacts.Reports.JUnit,
			CachePaths:            spec.Cache.Paths,
			CacheKey:              spec.Cache.Key.Literal,
			CacheKeyFiles:         spec.Cache.Key.Files,
			CachePolicy:           cachePolicy,
			TimeoutSec:            timeoutSec,
			Retry:                 retry,
			Manual:                inst.manual,
			AllowFailure:          inst.allowFailure,
			Services:              services,
		})
	}
	return out, nil
}
