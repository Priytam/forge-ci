// Package cloudrun is Forge Live's Cloud Run provider: each service is an
// application, its revisions its children, traffic split and conditions
// mapped to health.
//
// Phase A reconciles: one services.list per region every Period (shorter
// while a rollout is in flight) and, only for a service whose fingerprint
// changed, one revisions.list. Every call goes through a per-credential
// token bucket, so the cost is bounded whatever the fleet does. The
// event-driven design (Cloud Audit Logs → Pub/Sub → agent) replaces the
// timer later; the reported model does not change.
package cloudrun

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/oauth2"

	"github.com/priytamjeepandey/forge-ci/internal/live"
)

// Config is one project's regions.
type Config struct {
	Project string
	Regions []string
	// Period between reconciles (default 2m); 15s while a service is rolling out.
	Period time.Duration
	// CallsPerMinute is the credential's budget (default 60).
	CallsPerMinute int
	// BaseURL of the Cloud Run Admin API (tests point it at a fake).
	BaseURL string
	Tokens  oauth2.TokenSource
	Client  *http.Client
	// AllowSync lets Forge change services: set the deployed image, move
	// traffic back to a revision (needs roles/run.developer).
	AllowSync bool
}

// Provider reconciles Cloud Run services.
type Provider struct {
	cfg    Config
	bucket *bucket
	calls  atomic.Int64
	kick   chan struct{} // reconcile now (a refresh, after a change)
}

// New returns a provider for cfg.
func New(cfg Config) (*Provider, error) {
	if cfg.Project == "" || len(cfg.Regions) == 0 {
		return nil, errors.New("cloudrun: project and at least one region are required")
	}
	if cfg.Period <= 0 {
		cfg.Period = 2 * time.Minute
	}
	if cfg.CallsPerMinute <= 0 {
		cfg.CallsPerMinute = 60
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://run.googleapis.com"
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 30 * time.Second}
	}
	return &Provider{cfg: cfg, bucket: newBucket(cfg.CallsPerMinute), kick: make(chan struct{}, 1)}, nil
}

func (p *Provider) Name() string { return "cloudrun" }

func (p *Provider) Capabilities() live.Capabilities {
	return live.Capabilities{Manifests: true, ReconcileSeconds: int(math.Ceil(p.cfg.Period.Seconds())),
		Sync: p.cfg.AllowSync, Rollback: p.cfg.AllowSync}
}

// Calls is how many API calls the provider has made (the agent reports it).
func (p *Provider) Calls() int64 { return p.calls.Load() }

// service is the part of a Cloud Run v2 Service Forge reads; the raw JSON
// is the manifest.
type service struct {
	Name                string            `json:"name"`
	UID                 string            `json:"uid"`
	Labels              map[string]string `json:"labels"`
	CreateTime          time.Time         `json:"createTime"`
	Etag                string            `json:"etag"`
	Reconciling         bool              `json:"reconciling"`
	URI                 string            `json:"uri"`
	Ingress             string            `json:"ingress"`
	LatestReadyRevision string            `json:"latestReadyRevision"`
	LatestCreated       string            `json:"latestCreatedRevision"`
	TerminalCondition   condition         `json:"terminalCondition"`
	Template            struct {
		Containers []struct {
			Image string `json:"image"`
		} `json:"containers"`
	} `json:"template"`
	TrafficStatuses []struct {
		Revision string `json:"revision"`
		Percent  int    `json:"percent"`
		Tag      string `json:"tag"`
		Type     string `json:"type"`
	} `json:"trafficStatuses"`
	raw json.RawMessage
}

type revision struct {
	Name        string      `json:"name"`
	UID         string      `json:"uid"`
	CreateTime  time.Time   `json:"createTime"`
	Reconciling bool        `json:"reconciling"`
	Conditions  []condition `json:"conditions"`
	Containers  []struct {
		Image string `json:"image"`
	} `json:"containers"`
	Scaling struct {
		MinInstanceCount int `json:"minInstanceCount"`
		MaxInstanceCount int `json:"maxInstanceCount"`
	} `json:"scaling"`
	raw json.RawMessage
}

type condition struct {
	Type    string `json:"type"`
	State   string `json:"state"` // CONDITION_SUCCEEDED | CONDITION_FAILED | CONDITION_PENDING | CONDITION_RECONCILING
	Message string `json:"message"`
	Reason  string `json:"reason"`
}

// known is what the last reconcile reported for one service.
type known struct {
	fingerprint string
	app         live.App
	uids        []string
}

// Watch reconciles until ctx ends.
func (p *Provider) Watch(ctx context.Context, sink live.Sink) error {
	seen := map[string]*known{}
	first := true
	for {
		rolling, err := p.reconcile(ctx, sink, seen)
		if err != nil && ctx.Err() == nil {
			// Keep what was reported; the next reconcile tries again.
			slog.Warn("cloudrun: reconcile failed", "err", err)
		}
		if first && err == nil {
			sink.Synced()
			first = false
		}
		wait := p.cfg.Period
		if rolling {
			wait = min(wait, 15*time.Second)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-p.kick:
		case <-time.After(wait):
		}
	}
}

// reconcile lists every region's services and refreshes the ones whose
// fingerprint changed. rolling reports whether any service is mid-rollout.
func (p *Provider) reconcile(ctx context.Context, sink live.Sink, seen map[string]*known) (rolling bool, err error) {
	present := map[string]bool{}
	for _, region := range p.cfg.Regions {
		var svcs []service
		if err := p.list(ctx, fmt.Sprintf("/v2/projects/%s/locations/%s/services", p.cfg.Project, region), "services", &svcs); err != nil {
			return false, err
		}
		for _, s := range svcs {
			key := region + "/" + shortName(s.Name)
			present[key] = true
			rolling = rolling || s.Reconciling
			fp := fingerprint(s.raw)
			if k := seen[key]; k != nil && k.fingerprint == fp {
				continue
			}
			var revs []revision
			if err := p.list(ctx, "/v2/"+s.Name+"/revisions", "revisions", &revs); err != nil {
				return rolling, err
			}
			app := live.App{Key: key, Name: shortName(s.Name), Namespace: region, Group: s.Labels["part-of"]}
			uids := report(sink, app, s, revs)
			if k := seen[key]; k != nil {
				for _, uid := range k.uids {
					if !contains(uids, uid) {
						sink.Delete(app, uid)
					}
				}
			}
			seen[key] = &known{fingerprint: fp, app: app, uids: uids}
		}
	}
	for key, k := range seen {
		if !present[key] {
			for _, uid := range k.uids {
				sink.Delete(k.app, uid)
			}
			delete(seen, key)
		}
	}
	return rolling, nil
}

// maxRevisions bounds the revisions shown per service: those taking traffic,
// the latest created and ready, then the newest others.
const maxRevisions = 5

// report upserts the service and its revisions; it returns their uids.
func report(sink live.Sink, app live.App, s service, revs []revision) []string {
	traffic := map[string][]string{} // revision → "70%", "tag canary"
	for _, t := range s.TrafficStatuses {
		rev := shortName(t.Revision)
		if t.Type == "TRAFFIC_TARGET_ALLOCATION_TYPE_LATEST" && rev == "" {
			rev = shortName(s.LatestReadyRevision)
		}
		if t.Percent > 0 {
			traffic[rev] = append(traffic[rev], fmt.Sprintf("%d%%", t.Percent))
		}
		if t.Tag != "" {
			traffic[rev] = append(traffic[rev], "tag "+t.Tag)
		}
	}
	svc := live.Resource{Group: "run.googleapis.com", Version: "v2", Kind: "Service", Namespace: app.Namespace, Name: app.Name,
		UID: s.UID, CreatedAt: s.CreateTime.UTC(), Health: serviceHealth(s), Manifest: s.raw}
	for _, c := range s.Template.Containers {
		svc.Summary.Images = append(svc.Summary.Images, c.Image)
	}
	svc.Summary.Info = facts("URL", s.URI, "Ingress", strings.TrimPrefix(s.Ingress, "INGRESS_TRAFFIC_"), "Latest ready", shortName(s.LatestReadyRevision))
	sink.Upsert(app, svc)
	uids := []string{s.UID}

	sort.Slice(revs, func(i, j int) bool { return revs[i].CreateTime.After(revs[j].CreateTime) })
	shown := 0
	for _, r := range revs {
		name := shortName(r.Name)
		keep := traffic[name] != nil || name == shortName(s.LatestCreated) || name == shortName(s.LatestReadyRevision)
		if !keep && shown >= maxRevisions {
			continue
		}
		shown++
		res := live.Resource{Group: "run.googleapis.com", Version: "v2", Kind: "Revision", Namespace: app.Namespace, Name: name,
			UID: r.UID, Parents: []string{s.UID}, CreatedAt: r.CreateTime.UTC(), Health: revisionHealth(r), Manifest: r.raw}
		for _, c := range r.Containers {
			res.Summary.Images = append(res.Summary.Images, c.Image)
		}
		res.Summary.Status = "Inactive"
		if t := traffic[name]; t != nil {
			res.Summary.Status = "Serving " + strings.Join(t, ", ")
		}
		res.Summary.Info = facts("Traffic", strings.Join(traffic[name], ", "),
			"Instances", fmt.Sprintf("min %d, max %d", r.Scaling.MinInstanceCount, r.Scaling.MaxInstanceCount))
		sink.Upsert(app, res)
		uids = append(uids, r.UID)
	}
	return uids
}

func serviceHealth(s service) *live.Health {
	switch {
	case s.Reconciling:
		return &live.Health{Status: live.Progressing, Message: "Deploying " + shortName(s.LatestCreated)}
	case s.TerminalCondition.State == "CONDITION_FAILED":
		return &live.Health{Status: live.Degraded, Message: s.TerminalCondition.Message}
	case s.TerminalCondition.State == "CONDITION_SUCCEEDED":
		return &live.Health{Status: live.Healthy}
	}
	return &live.Health{Status: live.Progressing, Message: s.TerminalCondition.Message}
}

func revisionHealth(r revision) *live.Health {
	if r.Reconciling {
		return &live.Health{Status: live.Progressing, Message: "Starting"}
	}
	ready := false
	for _, c := range r.Conditions {
		switch c.Type {
		case "Ready", "ContainerHealthy", "ResourcesAvailable":
			if c.State == "CONDITION_FAILED" {
				return &live.Health{Status: live.Degraded, Message: strings.TrimSpace(c.Reason + " " + c.Message)}
			}
			if c.Type == "Ready" && c.State == "CONDITION_SUCCEEDED" {
				ready = true
			}
		}
	}
	if ready {
		return &live.Health{Status: live.Healthy}
	}
	return &live.Health{Status: live.Progressing, Message: "Waiting for the revision to be ready"}
}

// list pages through a list call, decoding items of field into out while
// keeping each item's raw JSON as its manifest.
func (p *Provider) list(ctx context.Context, path, field string, out any) error {
	var raws []json.RawMessage
	token := ""
	for {
		u := p.cfg.BaseURL + path + "?pageSize=500"
		if token != "" {
			u += "&pageToken=" + url.QueryEscape(token)
		}
		var page map[string]json.RawMessage
		if err := p.get(ctx, u, &page); err != nil {
			return err
		}
		var items []json.RawMessage
		if b := page[field]; b != nil {
			if err := json.Unmarshal(b, &items); err != nil {
				return err
			}
		}
		raws = append(raws, items...)
		token = ""
		if b := page["nextPageToken"]; b != nil {
			_ = json.Unmarshal(b, &token)
		}
		if token == "" {
			break
		}
	}
	switch o := out.(type) {
	case *[]service:
		for _, raw := range raws {
			var s service
			if err := json.Unmarshal(raw, &s); err != nil {
				return err
			}
			s.raw = raw
			*o = append(*o, s)
		}
	case *[]revision:
		for _, raw := range raws {
			var r revision
			if err := json.Unmarshal(raw, &r); err != nil {
				return err
			}
			r.raw = raw
			*o = append(*o, r)
		}
	}
	return nil
}

func (p *Provider) get(ctx context.Context, u string, out any) error {
	return p.do(ctx, http.MethodGet, u, nil, out)
}

// do is one API call under the budget.
func (p *Provider) do(ctx context.Context, method, u string, body []byte, out any) error {
	if err := p.bucket.take(ctx); err != nil {
		return err
	}
	p.calls.Add(1)
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if p.cfg.Tokens != nil {
		tok, err := p.cfg.Tokens.Token()
		if err != nil {
			return fmt.Errorf("cloudrun: token: %w", err)
		}
		tok.SetAuthHeader(req)
	}
	resp, err := p.cfg.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("cloudrun: %s %s: HTTP %d: %s", method, u, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// fingerprint changes whenever anything about the service does (its etag
// alone does not cover every status change).
func fingerprint(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:8])
}

func shortName(name string) string { return name[strings.LastIndex(name, "/")+1:] }

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

func facts(pairs ...string) []live.KV {
	var out []live.KV
	for i := 0; i+1 < len(pairs); i += 2 {
		if pairs[i+1] != "" {
			out = append(out, live.KV{Name: pairs[i], Value: pairs[i+1]})
		}
	}
	return out
}

// bucket is the credential's call budget: perMinute tokens, refilled
// continuously. A call waits for a token; the budget is never exceeded.
type bucket struct {
	perMinute float64
	tokens    float64
	last      time.Time
}

func newBucket(perMinute int) *bucket {
	return &bucket{perMinute: float64(perMinute), tokens: float64(perMinute), last: time.Now()}
}

func (b *bucket) take(ctx context.Context) error {
	for {
		now := time.Now()
		b.tokens = min(b.perMinute, b.tokens+now.Sub(b.last).Minutes()*b.perMinute)
		b.last = now
		if b.tokens >= 1 {
			b.tokens--
			return nil
		}
		wait := time.Duration((1 - b.tokens) / b.perMinute * float64(time.Minute))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

// Operate changes a service: sync sets the image Forge deployed, rollback
// sends all traffic to an earlier revision, refresh reconciles at once.
// Each is one read and one write of the service (a dry run validates only).
func (p *Provider) Operate(ctx context.Context, op live.Op, _ func([]byte)) live.OpResult {
	if op.Kind == live.OpRefresh {
		select {
		case p.kick <- struct{}{}:
		default:
		}
		return live.OpResult{OK: true, Message: "Reconciling now"}
	}
	if !p.cfg.AllowSync {
		return live.OpResult{Message: "this agent may not change services (--allow-sync)"}
	}
	region, name, ok := strings.Cut(op.App.Key, "/")
	if !ok {
		return live.OpResult{Message: "bad application key " + op.App.Key}
	}
	u := fmt.Sprintf("%s/v2/projects/%s/locations/%s/services/%s", p.cfg.BaseURL, p.cfg.Project, region, name)
	var svc map[string]any
	if err := p.get(ctx, u, &svc); err != nil {
		return live.OpResult{Message: err.Error()}
	}
	rr := live.ResourceResult{Key: op.App.Key, Kind: "Service", Name: name, Action: "configured"}
	switch op.Kind {
	case live.OpSync:
		tmpl, _ := svc["template"].(map[string]any)
		cs, _ := tmpl["containers"].([]any)
		if len(cs) == 0 {
			return live.OpResult{Message: "the service has no container"}
		}
		c := cs[0].(map[string]any)
		if c["image"] == op.Image {
			rr.Action = "unchanged"
		}
		c["image"] = op.Image
	case live.OpRollback:
		svc["traffic"] = []any{map[string]any{"type": "TRAFFIC_TARGET_ALLOCATION_TYPE_REVISION", "revision": op.Revision, "percent": 100}}
	default:
		return live.OpResult{Message: "Cloud Run cannot " + op.Kind}
	}
	if rr.Action != "unchanged" {
		body, _ := json.Marshal(svc)
		q := ""
		if op.DryRun {
			q, rr.Action = "?validateOnly=true", "would-configure"
		}
		if err := p.do(ctx, http.MethodPatch, u+q, body, nil); err != nil {
			rr.Action, rr.Message = "failed", err.Error()
			return live.OpResult{Message: err.Error(), Resources: []live.ResourceResult{rr}}
		}
		select {
		case p.kick <- struct{}{}:
		default:
		}
	}
	msg := "Set " + name + " to " + op.Image
	if op.Kind == live.OpRollback {
		msg = "Sent all of " + name + "'s traffic to " + op.Revision
	}
	return live.OpResult{OK: true, Message: msg, Revision: op.Revision, Resources: []live.ResourceResult{rr}}
}
