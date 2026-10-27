// Package live is Forge Live (batch 9): the provider-neutral model of an
// application running somewhere — a Kubernetes cluster, a Cloud Run region —
// and the wire format forge-agent uses to report it.
//
// Providers run in forge-agent, next to their credentials. They report each
// resource with its health already computed; the Tracker groups resources by
// application, coalesces bursts and hands the agent compact per-app deltas.
// The server stores deltas and serves pages from Postgres, the blob store and
// an in-memory LRU: it never calls a cluster or cloud API.
package live

import (
	"encoding/json"
	"iter"
	"sort"
	"time"
)

// Health statuses, from best to worst (Argo CD's well-known order, so people
// read them the same way).
const (
	Healthy     = "Healthy"
	Suspended   = "Suspended"
	Progressing = "Progressing"
	Missing     = "Missing"
	Degraded    = "Degraded"
	Unknown     = "Unknown"
)

var healthRank = map[string]int{Healthy: 0, Suspended: 1, Progressing: 2, Missing: 3, Degraded: 4, Unknown: 5}

// ValidHealth reports whether s is one of the health statuses.
func ValidHealth(s string) bool { _, ok := healthRank[s]; return ok }

// Worse reports whether health a is worse than b.
func Worse(a, b string) bool { return healthRank[a] > healthRank[b] }

// Health is a resource's (or an application's) health and why.
type Health struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// KV is one fact a resource's summary shows ("Type": "ClusterIP").
type KV struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Summary is what a node shows at a glance.
type Summary struct {
	Ready    string   `json:"ready,omitempty"`  // "2/3": ready of desired replicas or containers
	Status   string   `json:"status,omitempty"` // a pod's state in kubectl's words (Running, CrashLoopBackOff)
	Restarts int      `json:"restarts,omitempty"`
	Images   []string `json:"images,omitempty"`
	Info     []KV     `json:"info,omitempty"`
}

// Resource is one live object of an application, as the agent sees it.
type Resource struct {
	Group     string `json:"group,omitempty"`
	Version   string `json:"version,omitempty"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
	// Parents are the uids of the resources that own this one within the
	// application (a Pod's ReplicaSet); none for the application's top level.
	Parents   []string  `json:"parents,omitempty"`
	Health    *Health   `json:"health,omitempty"` // nil: the kind has no health (a ConfigMap)
	Summary   Summary   `json:"summary"`
	CreatedAt time.Time `json:"created_at"`
	// Manifest is the live object, trimmed (no managed fields, no
	// last-applied annotation, large data values replaced by their size).
	// It travels in deltas only: the agent does not keep it, the server
	// stores it apart from the tree.
	Manifest json.RawMessage `json:"manifest,omitempty"`
}

// App identifies an application within a target.
type App struct {
	Key       string `json:"key"`                 // unique per target: namespace/name, region/service
	Name      string `json:"name"`                // what people call it
	Namespace string `json:"namespace,omitempty"` // Kubernetes namespace, or cloud region
	Group     string `json:"group,omitempty"`     // the system it is part of (app.kubernetes.io/part-of)
}

// Status is an application's state in one line: what the Applications page
// lists and filters on.
type Status struct {
	Health    Health   `json:"health"`
	Resources int      `json:"resources"`
	Pods      int      `json:"pods"`
	PodsReady int      `json:"pods_ready"`
	Images    []string `json:"images"`
}

// Event is something the platform said about a resource (Kubernetes events,
// Cloud Run conditions).
type Event struct {
	UID         string    `json:"uid"`
	ResourceUID string    `json:"resource_uid"`
	Kind        string    `json:"kind"`
	Name        string    `json:"name"`
	Type        string    `json:"type"` // Normal | Warning
	Reason      string    `json:"reason"`
	Message     string    `json:"message"`
	Count       int       `json:"count"`
	FirstSeen   time.Time `json:"first_seen"`
	LastSeen    time.Time `json:"last_seen"`
}

// StatusOf summarises an application's resources: its health is the worst of
// any resource's (so one crash-looping pod makes the app Degraded at once),
// with that resource named in the message.
func StatusOf(resources iter.Seq[Resource]) Status {
	st := Status{Health: Health{Status: Healthy}, Images: []string{}}
	images := map[string]bool{}
	worst := Resource{}
	for r := range resources {
		st.Resources++
		if r.Kind == "Pod" {
			st.Pods++
			if r.Health != nil && r.Health.Status == Healthy {
				st.PodsReady++
			}
		}
		if len(r.Parents) == 0 {
			for _, img := range r.Summary.Images {
				images[img] = true
			}
		}
		if r.Health == nil || r.Health.Status == Healthy {
			continue
		}
		// On a tie the most specific resource speaks — a pod's
		// ImagePullBackOff says more than its Deployment's deadline — then
		// the first kind/name, so the message is stable.
		if worst.UID == "" || Worse(r.Health.Status, worst.Health.Status) ||
			(r.Health.Status == worst.Health.Status && moreSpecific(r, worst)) {
			worst = r
		}
	}
	if worst.UID != "" {
		st.Health = Health{Status: worst.Health.Status, Message: worst.Kind + " " + worst.Name}
		if worst.Health.Message != "" {
			st.Health.Message += ": " + worst.Health.Message
		}
	}
	for img := range images {
		st.Images = append(st.Images, img)
	}
	sort.Strings(st.Images)
	return st
}

func moreSpecific(a, b Resource) bool {
	if (len(a.Parents) > 0) != (len(b.Parents) > 0) {
		return len(a.Parents) > 0
	}
	return a.Kind+"/"+a.Name < b.Kind+"/"+b.Name
}

// ---- wire format: agent → server ----

// Delta is one application's changes since the last batch. Deltas merge
// into what the server holds. Once per epoch, when the agent's initial
// report is complete, each app's delta carries Keep — every resource uid the
// app has — and the server drops anything else it still holds for the app.
type Delta struct {
	App App `json:"app"`
	// Status is nil while the initial report is still streaming (the agent
	// has seen only part of the app, so its health would be a guess).
	Status  *Status    `json:"status,omitempty"`
	Upserts []Resource `json:"upserts,omitempty"`
	Deletes []string   `json:"deletes,omitempty"` // uids
	Keep    []string   `json:"keep,omitempty"`
	Events  []Event    `json:"events,omitempty"`
	Gone    bool       `json:"gone,omitempty"` // the app has no resources left
}

// Capabilities says what a provider can show; the UI branches on these,
// never on the provider's name.
type Capabilities struct {
	Events    bool `json:"events"`    // the Events tab
	Manifests bool `json:"manifests"` // the Live manifest tab
	// Watch: changes arrive as they happen. Otherwise the provider
	// reconciles every ReconcileSeconds, under a call budget.
	Watch            bool `json:"watch"`
	ReconcileSeconds int  `json:"reconcile_seconds,omitempty"`
}

// AgentStats is what an agent reports about itself with every batch.
type AgentStats struct {
	Objects       int     `json:"objects"`        // resources tracked
	Apps          int     `json:"apps"`           // applications tracked
	HeapBytes     uint64  `json:"heap_bytes"`     // Go heap in use
	CPUSeconds    float64 `json:"cpu_seconds"`    // process CPU since start
	BytesSent     int64   `json:"bytes_sent"`     // request bodies since start (compressed)
	ProviderCalls int64   `json:"provider_calls"` // cloud API calls since start (0 for watch providers)
	Uptime        float64 `json:"uptime_seconds"`
}

// Batch is one POST from an agent. Epoch names one run of the provider (a
// fresh full report); Seq numbers batches within it. Together they are the
// resume token: a retried batch is acknowledged without being applied twice,
// and a gap makes the server ask for a fresh epoch.
type Batch struct {
	Epoch        string       `json:"epoch"`
	Seq          int64        `json:"seq"`
	Provider     string       `json:"provider"`
	Capabilities Capabilities `json:"capabilities"`
	Version      string       `json:"version"`
	// Synced: the initial report is complete (every app has had its Keep),
	// so the server drops applications the epoch did not mention.
	Synced bool       `json:"synced,omitempty"`
	Deltas []Delta    `json:"deltas,omitempty"`
	Stats  AgentStats `json:"stats"`
}

// Ack is the server's answer to a batch.
type Ack struct {
	Seq int64 `json:"seq"`
	// Resync asks the agent for a fresh epoch (the server lost track, or a
	// batch is missing).
	Resync bool `json:"resync,omitempty"`
}
