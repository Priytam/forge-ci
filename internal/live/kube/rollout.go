package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/priytamjeepandey/forge-ci/internal/live"
)

// Progressive delivery (phase D): canary rollouts, run by the agent with no
// controller or CRD in the cluster. A Rollout lives in Git next to the app:
//
//	apiVersion: forge.dev/v1
//	kind: Rollout
//	metadata: {name: cart-api}        # the Deployment it rolls out
//	spec:
//	  steps:                           # default: 25%, 1m, 50%, 1m
//	    - weight: 20
//	    - pause: {duration: 30s}       # pause: {} waits for Promote
//	    - weight: 50
//	    - pause: {duration: 30s}
//	  analysis:                        # runs in the background throughout
//	    interval: 10s                  # default 10s
//	    failureLimit: 2                # consecutive failed rounds that abort (default 2)
//	    health: {maxRestarts: 0}       # always on
//	    http: {path: /healthz, port: 8080, successRate: 0.95}
//	    prometheus: {address: http://prometheus:9090, query: '...', max: 0.05}
//
// It is never applied. When a sync changes that Deployment's pod template,
// the agent runs the new template as <name>-canary beside the stable
// Deployment — the app's Services select both, so traffic splits by pod
// count — and walks the steps while the analysis runs. Every step passed,
// it promotes: the stable Deployment gets the new template (a normal
// rolling update) and the canary goes. A failed analysis, or Abort, removes
// the canary and leaves the stable version untouched. Rollbacks and first
// deploys apply directly: they are not the time to go slowly.

const (
	rolloutAPIVersion = "forge.dev/v1"
	annCanaryOf       = annotationPrefix + "canary-of"
	annCanaryRev      = annotationPrefix + "canary-revision"
	labelTrack        = annotationPrefix + "track"
	canarySuffix      = "-canary"
)

// Duration is a duration written as Go writes it ("30s", "2m").
type Duration time.Duration

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		var n float64
		if err := json.Unmarshal(b, &n); err != nil {
			return fmt.Errorf("a duration like \"30s\": %s", b)
		}
		*d = Duration(time.Duration(n) * time.Second)
		return nil
	}
	v, err := time.ParseDuration(s)
	*d = Duration(v)
	return err
}

func (d Duration) String() string { return time.Duration(d).String() }

// RolloutSpec is a Rollout's spec.
type RolloutSpec struct {
	Steps    []RolloutStep `json:"steps"`
	Analysis Analysis      `json:"analysis"`
}

// RolloutStep is one step: a canary weight, or a pause.
type RolloutStep struct {
	Weight int    `json:"weight,omitempty"` // percent of the app's pods
	Pause  *Pause `json:"pause,omitempty"`
}

// Pause waits Duration, or (zero) until someone promotes.
type Pause struct {
	Duration Duration `json:"duration,omitempty"`
}

// Analysis is what decides whether a canary is good.
type Analysis struct {
	Interval     Duration    `json:"interval"`
	FailureLimit int         `json:"failureLimit"`
	Health       HealthCheck `json:"health"`
	HTTP         *HTTPCheck  `json:"http,omitempty"`
	Prometheus   *PromCheck  `json:"prometheus,omitempty"`
}

// HealthCheck fails when a canary pod is Degraded or restarted.
type HealthCheck struct {
	MaxRestarts int `json:"maxRestarts"`
}

// HTTPCheck sends requests to each ready canary pod (the agent runs in the
// cluster, so it reaches pod IPs) and wants enough of them to succeed.
type HTTPCheck struct {
	Path        string  `json:"path"`
	Port        int     `json:"port"`        // default: the first container port, else 8080
	SuccessRate float64 `json:"successRate"` // default 0.95
	Requests    int     `json:"requests"`    // per pod per round, default 5
}

// PromCheck runs an instant query and wants its value within bounds.
// {{canary}} and {{namespace}} in the query are replaced.
type PromCheck struct {
	Address string   `json:"address"`
	Query   string   `json:"query"`
	Max     *float64 `json:"max,omitempty"`
	Min     *float64 `json:"min,omitempty"`
}

var defaultSteps = []RolloutStep{
	{Weight: 25}, {Pause: &Pause{Duration: Duration(time.Minute)}},
	{Weight: 50}, {Pause: &Pause{Duration: Duration(time.Minute)}},
}

// rollouts takes the Rollouts out of a rendered app: they are read, never
// applied. It returns the other objects and the Rollouts by Deployment name.
func rollouts(objs []*unstructured.Unstructured) ([]*unstructured.Unstructured, map[string]*RolloutSpec, error) {
	out := objs[:0:0]
	specs := map[string]*RolloutSpec{}
	for _, o := range objs {
		if o.GetAPIVersion() != rolloutAPIVersion {
			out = append(out, o)
			continue
		}
		if o.GetKind() != "Rollout" {
			return nil, nil, fmt.Errorf("%s %s: Forge knows kind Rollout only", rolloutAPIVersion, o.GetKind())
		}
		raw, _ := json.Marshal(o.Object["spec"])
		spec := &RolloutSpec{}
		if string(raw) != "null" {
			if err := json.Unmarshal(raw, spec); err != nil {
				return nil, nil, fmt.Errorf("Rollout %s: %w", o.GetName(), err)
			}
		}
		if err := spec.normalize(); err != nil {
			return nil, nil, fmt.Errorf("Rollout %s: %w", o.GetName(), err)
		}
		specs[o.GetName()] = spec
	}
	return out, specs, nil
}

func (r *RolloutSpec) normalize() error {
	if len(r.Steps) == 0 {
		r.Steps = defaultSteps
	}
	for i, s := range r.Steps {
		if (s.Weight > 0) == (s.Pause != nil) {
			return fmt.Errorf("step %d: give a weight or a pause", i+1)
		}
		if s.Weight < 0 || s.Weight > 100 {
			return fmt.Errorf("step %d: weight %d is not a percentage", i+1, s.Weight)
		}
	}
	a := &r.Analysis
	if a.Interval <= 0 {
		a.Interval = Duration(10 * time.Second)
	}
	if a.FailureLimit <= 0 {
		a.FailureLimit = 2
	}
	if h := a.HTTP; h != nil {
		if h.Path == "" {
			h.Path = "/"
		}
		if h.SuccessRate <= 0 {
			h.SuccessRate = 0.95
		}
		if h.Requests <= 0 {
			h.Requests = 5
		}
	}
	if p := a.Prometheus; p != nil && (p.Address == "" || p.Query == "" || p.Max == nil && p.Min == nil) {
		return fmt.Errorf("analysis.prometheus needs address, query and max or min")
	}
	return nil
}

// canaryReplicas is how many canary pods put weight percent of the app's
// pods on the canary while the stable Deployment keeps all of its own.
func canaryReplicas(stable int64, weight int) int64 {
	if stable < 1 {
		stable = 1
	}
	c := int64(math.Round(float64(stable) * float64(weight) / float64(100-weight)))
	return max(c, 1)
}

// ---- running one ----

// rolloutCtl lets promote and abort operations reach a running rollout.
type rolloutCtl struct {
	signal chan live.Op
}

func (p *Provider) rolloutCtl(app string) (*rolloutCtl, func()) {
	p.rmu.Lock()
	defer p.rmu.Unlock()
	if p.running == nil {
		p.running = map[string]*rolloutCtl{}
	}
	c := &rolloutCtl{signal: make(chan live.Op, 1)}
	p.running[app] = c
	return c, func() {
		p.rmu.Lock()
		defer p.rmu.Unlock()
		if p.running[app] == c {
			delete(p.running, app)
		}
	}
}

// signal hands a promote or abort to app's running rollout; ok=false when
// none runs here.
func (p *Provider) signal(op live.Op) bool {
	p.rmu.Lock()
	c := p.running[op.App.Key]
	p.rmu.Unlock()
	if c == nil {
		return false
	}
	select {
	case c.signal <- op:
	default: // one is already waiting to be read
	}
	return true
}

// podLister lists pods by selector (the analysis's view of the canary).
type podLister func(ctx context.Context, namespace, selector string) ([]corev1.Pod, error)

// rollout runs desired (a Deployment with a Rollout) as a canary. It returns
// what happened to the Deployment; ok=false stops the sync.
func (s *syncRun) rollout(ctx context.Context, desired *unstructured.Unstructured, spec *RolloutSpec, rev string) (live.ResourceResult, bool) {
	rr := live.ResourceResult{Key: keyOf(desired), Kind: desired.GetKind(), Name: desired.GetName()}
	name, ns := desired.GetName(), desired.GetNamespace()
	stable, err := s.applier.Get(ctx, "apps", "v1", "Deployment", ns, name)
	if err != nil {
		rr.Action, rr.Message = "failed", err.Error()
		return rr, false
	}
	if stable == nil || sameTemplate(desired, stable) {
		// A first deploy, or nothing about the pods changes: no canary.
		action, err := s.applier.Apply(ctx, desired, false, s.op.Force)
		if err != nil {
			rr.Action, rr.Message = "failed", err.Error()
			return rr, false
		}
		rr.Action = action
		return rr, true
	}
	n := int64(1)
	switch v, _, _ := unstructured.NestedFieldNoCopy(stable.Object, "spec", "replicas"); v := v.(type) {
	case int64:
		n = v
	case float64: // decoded from YAML or JSON
		n = int64(v)
	}
	canary, err := canaryOf(desired, rev)
	if err != nil {
		rr.Action, rr.Message = "failed", err.Error()
		return rr, false
	}
	ctl, done := s.w.p.rolloutCtl(s.app.Key)
	defer done()
	sel, _, _ := unstructured.NestedStringMap(canary.Object, "spec", "selector", "matchLabels")
	r := &canaryRun{s: s, spec: spec, canary: canary, stable: n, ctl: ctl, rev: rev,
		name: canary.GetName(), ns: ns, selector: labels.SelectorFromSet(sel).String()}
	ok, msg := r.run(ctx)
	rr.Message = msg
	switch {
	case ok:
		// Promote: the stable Deployment takes the new template.
		s.say("Promoting %s: the stable Deployment rolls out %s", name, shortSHA(rev))
		action, err := s.applier.Apply(ctx, desired, false, s.op.Force)
		if err == nil {
			err = s.waitHealthy(ctx, desired)
		}
		if err != nil {
			rr.Action, rr.Message = "failed", "promote: "+err.Error()
			r.remove(ctx)
			return rr, false
		}
		r.remove(ctx)
		rr.Action, rr.Message = "promoted", msg
		if action == "unchanged" {
			rr.Message += " (the stable Deployment already had it)"
		}
		return rr, true
	default:
		r.remove(ctx)
		rr.Action = "aborted"
		s.res.Aborted = true
		s.fail = fmt.Sprintf("Rollout of %s aborted: %s. %s still runs the stable version.", name, msg, name)
		return rr, false
	}
}

// sameTemplate: the live Deployment already runs the desired pod template.
func sameTemplate(desired, live *unstructured.Unstructured) bool {
	d, _, _ := unstructured.NestedMap(desired.Object, "spec", "template")
	l, _, _ := unstructured.NestedMap(live.Object, "spec", "template")
	ok, _ := inSync(d, l)
	return ok
}

// canaryOf is desired as its canary: <name>-canary, pods labelled
// forge.dev/track=canary (the stable Deployment's selector still matches
// them, so its Services send them traffic), starting at no replicas.
func canaryOf(desired *unstructured.Unstructured, rev string) (*unstructured.Unstructured, error) {
	c := desired.DeepCopy()
	c.SetName(desired.GetName() + canarySuffix)
	ann := c.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	delete(ann, annWave)
	ann[annCanaryOf], ann[annCanaryRev] = desired.GetName(), rev
	c.SetAnnotations(ann)
	for _, path := range [][]string{{"spec", "selector", "matchLabels"}, {"spec", "template", "metadata", "labels"}} {
		m, _, err := unstructured.NestedStringMap(c.Object, path...)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", strings.Join(path, "."), err)
		}
		if m == nil {
			m = map[string]string{}
		}
		m[labelTrack] = "canary"
		if err := unstructured.SetNestedStringMap(c.Object, m, path...); err != nil {
			return nil, err
		}
	}
	unstructured.RemoveNestedField(c.Object, "status")
	return c, nil
}

// canaryRun is one canary in progress.
type canaryRun struct {
	s      *syncRun
	spec   *RolloutSpec
	canary *unstructured.Unstructured // its replicas change step by step: the analysis reads name, ns and selector
	stable int64
	// Fixed for the run, for the analysis goroutine.
	name, ns, selector string
	ctl                *rolloutCtl
	rev                string

	mu      sync.Mutex
	fails   int    // consecutive failed analysis rounds
	verdict string // why the analysis failed for good ("" = still fine)
	rounds  int
	resumed bool // a Resume arrived before the manual pause it is for
}

// run walks the steps. ok: promote; otherwise msg says why it stopped.
func (r *canaryRun) run(ctx context.Context) (ok bool, msg string) {
	s := r.s
	name := r.canary.GetName()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	failed := make(chan struct{})
	started := false
	weights := []string{}
	for i, st := range r.spec.Steps {
		switch {
		case st.Weight >= 100:
			s.say("Step %d: 100%% — promoting", i+1)
			return true, r.summary(weights, "promoted")
		case st.Weight > 0:
			c := canaryReplicas(r.stable, st.Weight)
			s.say("Step %d: %d%% — %s at %d replica%s beside %d stable (%d%% of pods)", i+1, st.Weight, name, c, plural(int(c)),
				r.stable, int(math.Round(100*float64(c)/float64(c+r.stable))))
			if err := unstructured.SetNestedField(r.canary.Object, c, "spec", "replicas"); err != nil {
				return false, err.Error()
			}
			if _, err := s.applier.Apply(ctx, r.canary, false, true); err != nil {
				return false, "apply the canary: " + err.Error()
			}
			weights = append(weights, strconv.Itoa(st.Weight)+"%")
			if !started {
				started = true
				go r.analyze(ctx, failed)
			}
			if stop, why := r.waitReady(ctx, failed); stop {
				return why == "promote", r.summary(weights, why)
			}
		default:
			if st.Pause.Duration > 0 {
				s.say("Step %d: pause %s", i+1, st.Pause.Duration)
			} else {
				s.say("Step %d: paused until someone promotes", i+1)
			}
			if stop, why := r.pause(ctx, time.Duration(st.Pause.Duration), failed); stop {
				return why == "promote", r.summary(weights, why)
			}
		}
	}
	if r.failed() {
		return false, r.summary(weights, "")
	}
	return true, r.summary(weights, "promoted")
}

func (r *canaryRun) summary(weights []string, why string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	path, at := strings.Join(weights, " → "), "0%"
	if len(weights) > 0 {
		at = weights[len(weights)-1]
	} else {
		path = "0%"
	}
	switch {
	case r.verdict != "":
		return fmt.Sprintf("analysis failed at %s: %s", at, r.verdict)
	case why == "abort":
		return "aborted by hand at " + at
	case why == "promote":
		return fmt.Sprintf("%s, then promoted by hand; analysis passed %d round%s", path, r.rounds, plural(r.rounds))
	case why == "promoted":
		return fmt.Sprintf("%s → 100%%; analysis passed %d round%s", path, r.rounds, plural(r.rounds))
	}
	return why
}

func (r *canaryRun) failed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.verdict != ""
}

// waitReady waits for the canary's pods to be ready at this weight, while
// the analysis runs and promote/abort may arrive.
func (r *canaryRun) waitReady(ctx context.Context, failed <-chan struct{}) (stop bool, why string) {
	deadline := time.Now().Add(waveTimeout)
	for {
		if _, h, err := r.s.health(ctx, r.canary); err == nil && h != nil && h.Status == live.Healthy {
			return false, ""
		}
		if time.Now().After(deadline) {
			r.mu.Lock()
			r.verdict = fmt.Sprintf("the canary was not ready after %s", waveTimeout)
			r.mu.Unlock()
			return true, ""
		}
		if stop, why := r.pause(ctx, wavePoll, failed); stop {
			return true, why
		}
	}
}

// pause waits d (0: until promoted), stopping early for a failed analysis,
// a promote or an abort.
func (r *canaryRun) pause(ctx context.Context, d time.Duration, failed <-chan struct{}) (stop bool, why string) {
	var timer <-chan time.Time
	if d == 0 && r.resumed {
		r.resumed = false
		r.s.say("Resumed")
		return false, ""
	}
	if d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		timer = t.C
	}
	for {
		select {
		case <-ctx.Done():
			return true, "the sync was canceled"
		case <-failed:
			return true, ""
		case op := <-r.ctl.signal:
			if op.Kind == live.OpAbort {
				r.s.say("Abort requested by %s", requester(op))
				return true, "abort"
			}
			if op.Full {
				r.s.say("Promote requested by %s", requester(op))
				return true, "promote"
			}
			if d == 0 {
				r.s.say("Resumed by %s", requester(op))
				return false, ""
			}
			r.resumed = true // for the next manual pause
		case <-timer:
			return false, ""
		}
	}
}

func requester(op live.Op) string {
	if op.Trigger != "" {
		return op.Trigger
	}
	return "a person"
}

// remove deletes the canary (abort, or after promotion).
func (r *canaryRun) remove(ctx context.Context) {
	ctx = context.WithoutCancel(ctx) // an aborted sync still cleans up
	if err := r.s.applier.Delete(ctx, "apps", "v1", "Deployment", r.canary.GetNamespace(), r.canary.GetName(), false); err != nil {
		r.s.say("could not remove %s: %v", r.canary.GetName(), err)
		return
	}
	r.s.say("Removed %s", r.canary.GetName())
}

// analyze runs the analysis every interval until ctx ends; failureLimit
// failed rounds in a row close failed.
func (r *canaryRun) analyze(ctx context.Context, failed chan<- struct{}) {
	a := r.spec.Analysis
	tick := time.NewTicker(time.Duration(a.Interval))
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		problem, detail := r.round(ctx)
		r.mu.Lock()
		r.rounds++
		if problem == "" {
			r.fails = 0
		} else {
			r.fails++
		}
		n, fails := r.rounds, r.fails
		if problem != "" && fails >= a.FailureLimit {
			r.verdict = problem
		}
		done := r.verdict != ""
		r.mu.Unlock()
		switch {
		case problem == "":
			r.s.say("Analysis %d: passed (%s)", n, detail)
		default:
			r.s.say("Analysis %d: failed %d of %d: %s", n, fails, a.FailureLimit, problem)
		}
		if done {
			close(failed)
			return
		}
	}
}

// round is one analysis round: "" when the canary passes every check
// (detail says what was checked), else the first problem.
func (r *canaryRun) round(ctx context.Context) (problem, detail string) {
	ns := r.ns
	pods, err := r.s.pods(ctx, ns, r.selector)
	if err != nil {
		return "list canary pods: " + err.Error(), ""
	}
	a := r.spec.Analysis
	var parts []string
	ready := 0
	for i := range pods {
		p := &pods[i]
		h := podHealth(p)
		if h.Status == live.Degraded {
			return fmt.Sprintf("health: %s %s", p.Name, h.Message), ""
		}
		restarts := 0
		for _, c := range p.Status.ContainerStatuses {
			restarts += int(c.RestartCount)
		}
		if restarts > a.Health.MaxRestarts {
			return fmt.Sprintf("health: %s restarted %d time%s", p.Name, restarts, plural(restarts)), ""
		}
		if podReady(p) {
			ready++
		}
	}
	parts = append(parts, fmt.Sprintf("health: %d/%d pods ready", ready, len(pods)))
	if h := a.HTTP; h != nil {
		ok, total, why := r.httpRound(ctx, pods, h)
		if total > 0 {
			rate := float64(ok) / float64(total)
			if rate < h.SuccessRate {
				return fmt.Sprintf("http: %.0f%% of %d requests to %s succeeded, want %.0f%%%s", 100*rate, total, h.Path, 100*h.SuccessRate, why), ""
			}
			parts = append(parts, fmt.Sprintf("http: %d/%d", ok, total))
		}
	}
	if pc := a.Prometheus; pc != nil {
		v, err := promQuery(ctx, r.s.httpClient(), pc, r.name, ns)
		if err != nil {
			return "prometheus: " + err.Error(), ""
		}
		if pc.Max != nil && v > *pc.Max {
			return fmt.Sprintf("prometheus: %g > max %g", v, *pc.Max), ""
		}
		if pc.Min != nil && v < *pc.Min {
			return fmt.Sprintf("prometheus: %g < min %g", v, *pc.Min), ""
		}
		parts = append(parts, fmt.Sprintf("prometheus: %g", v))
	}
	return "", strings.Join(parts, ", ")
}

func (r *canaryRun) httpRound(ctx context.Context, pods []corev1.Pod, h *HTTPCheck) (ok, total int, why string) {
	client := r.s.httpClient()
	for i := range pods {
		p := &pods[i]
		if !podReady(p) || p.Status.PodIP == "" {
			continue
		}
		port := h.Port
		if port == 0 {
			port = 8080
			if cs := p.Spec.Containers; len(cs) > 0 && len(cs[0].Ports) > 0 {
				port = int(cs[0].Ports[0].ContainerPort)
			}
		}
		u := fmt.Sprintf("http://%s:%d%s", p.Status.PodIP, port, h.Path)
		for range h.Requests {
			total++
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
			resp, err := client.Do(req)
			if err != nil {
				why = " (" + err.Error() + ")"
				continue
			}
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
			if resp.StatusCode < 400 {
				ok++
			} else {
				why = fmt.Sprintf(" (last: HTTP %d)", resp.StatusCode)
			}
		}
	}
	return ok, total, why
}

func promQuery(ctx context.Context, client *http.Client, pc *PromCheck, canary, ns string) (float64, error) {
	q := strings.NewReplacer("{{canary}}", canary, "{{namespace}}", ns).Replace(pc.Query)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(pc.Address, "/")+"/api/v1/query?query="+url.QueryEscape(q), nil)
	if err != nil {
		return 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	var out struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			ResultType string          `json:"resultType"`
			Result     json.RawMessage `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return 0, fmt.Errorf("HTTP %d: unreadable answer", resp.StatusCode)
	}
	if out.Status != "success" {
		return 0, fmt.Errorf("%s", out.Error)
	}
	var sample []any
	switch out.Data.ResultType {
	case "scalar":
		_ = json.Unmarshal(out.Data.Result, &sample)
	case "vector":
		var v []struct {
			Value []any `json:"value"`
		}
		_ = json.Unmarshal(out.Data.Result, &v)
		if len(v) == 0 {
			return 0, fmt.Errorf("no data for %s", q)
		}
		sample = v[0].Value
	default:
		return 0, fmt.Errorf("want a scalar or vector, got %s", out.Data.ResultType)
	}
	if len(sample) != 2 {
		return 0, fmt.Errorf("no value in the answer")
	}
	str, _ := sample[1].(string)
	return strconv.ParseFloat(str, 64)
}

// waitHealthy waits for an applied object to be healthy (the stable
// Deployment's rolling update at promotion).
func (s *syncRun) waitHealthy(ctx context.Context, o *unstructured.Unstructured) error {
	deadline := time.Now().Add(waveTimeout)
	for {
		_, h, err := s.health(ctx, o)
		switch {
		case err != nil:
		case h == nil, h.Status == live.Healthy:
			return nil
		case h.Status == live.Degraded:
			return fmt.Errorf("%s %s is Degraded: %s", o.GetKind(), o.GetName(), h.Message)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s %s not healthy after %s", o.GetKind(), o.GetName(), waveTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wavePoll):
		}
	}
}
