package kube

import (
	"cmp"
	"slices"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Sync waves and hooks. A sync runs in phases — PreSync hooks, then the
// resources (and Sync hooks), then PostSync hooks — and within each phase in
// waves, lowest first. The agent waits for a wave to be healthy (and its
// hooks to complete) before the next one starts. When anything fails,
// SyncFail hooks run and the sync stops.
//
// Annotations (Forge's own; Helm's hook annotations are honoured too, so a
// chart's migration Job runs as a hook instead of drifting forever):
//
//	forge.dev/sync-wave: "-1"                      the wave (default 0)
//	forge.dev/hook: PreSync | Sync | PostSync | SyncFail
//	forge.dev/hook-delete-policy: BeforeHookCreation | HookSucceeded | HookFailed
const (
	annotationPrefix = "forge.dev/"
	annWave          = annotationPrefix + "sync-wave"
	annHook          = annotationPrefix + "hook"
	annHookDelete    = annotationPrefix + "hook-delete-policy"
)

// Sync phases.
const (
	preSync   = "PreSync"
	syncPhase = "Sync"
	postSync  = "PostSync"
	syncFail  = "SyncFail"
	// skipHook is a Helm hook Forge does not run on sync (test, delete,
	// rollback hooks): never applied, never compared.
	skipHook = "Skip"
)

// Hook delete policies.
const (
	BeforeHookCreation = "BeforeHookCreation"
	HookSucceeded      = "HookSucceeded"
	HookFailed         = "HookFailed"
)

// hook is what an object's annotations say about running it.
type hook struct {
	phase  string          // "" for an ordinary resource
	wave   int             // its wave within the phase
	delete map[string]bool // delete policies (hooks only)
}

var helmPhases = map[string]string{
	"pre-install": preSync, "pre-upgrade": preSync, "post-install": postSync, "post-upgrade": postSync,
}

var helmDelete = map[string]string{
	"before-hook-creation": BeforeHookCreation, "hook-succeeded": HookSucceeded, "hook-failed": HookFailed,
}

func hookOf(ann map[string]string) hook {
	h := hook{delete: map[string]bool{}}
	if w, err := strconv.Atoi(strings.TrimSpace(ann[annWave])); err == nil {
		h.wave = w
	}
	if v := ann[annHook]; v != "" {
		for _, p := range strings.Split(v, ",") {
			switch p = strings.TrimSpace(p); p {
			case preSync, syncPhase, postSync, syncFail:
				h.phase = p
			}
			if h.phase != "" {
				break
			}
		}
		if h.phase == "" {
			h.phase = skipHook // a hook Forge does not know: never run it as a resource
		}
		for _, p := range strings.Split(ann[annHookDelete], ",") {
			if p = strings.TrimSpace(p); p != "" {
				h.delete[p] = true
			}
		}
	} else if v := ann["helm.sh/hook"]; v != "" {
		h.phase = skipHook
		for _, p := range strings.Split(v, ",") {
			if ph, ok := helmPhases[strings.TrimSpace(p)]; ok {
				h.phase = ph
				break
			}
		}
		if w, err := strconv.Atoi(strings.TrimSpace(ann["helm.sh/hook-weight"])); err == nil {
			h.wave = w
		}
		for _, p := range strings.Split(ann["helm.sh/hook-delete-policy"], ",") {
			if d, ok := helmDelete[strings.TrimSpace(p)]; ok {
				h.delete[d] = true
			}
		}
	}
	if h.phase != "" && len(h.delete) == 0 {
		h.delete[BeforeHookCreation] = true // so the next sync can run it again
	}
	return h
}

// step is one wave of one phase: its hooks and resources, applied together
// in dependency order.
type step struct {
	phase string
	wave  int
	objs  []*unstructured.Unstructured
	hooks map[*unstructured.Unstructured]hook
}

var phaseOrder = map[string]int{preSync: 0, syncPhase: 1, postSync: 2}

// plan orders a sync: the steps of each phase, and the SyncFail hooks
// apart. Hooks Forge does not run are left out.
func plan(objs []*unstructured.Unstructured) (steps []*step, failHooks []*unstructured.Unstructured) {
	byStep := map[[2]int]*step{}
	for _, o := range objs {
		h := hookOf(o.GetAnnotations())
		phase := h.phase
		switch phase {
		case skipHook:
			continue
		case syncFail:
			failHooks = append(failHooks, o)
			continue
		case "":
			phase = syncPhase
		}
		k := [2]int{phaseOrder[phase], h.wave}
		s := byStep[k]
		if s == nil {
			s = &step{phase: phase, wave: h.wave, hooks: map[*unstructured.Unstructured]hook{}}
			byStep[k] = s
			steps = append(steps, s)
		}
		s.objs = append(s.objs, o)
		if h.phase != "" {
			s.hooks[o] = h
		}
	}
	slices.SortFunc(steps, func(a, b *step) int {
		return cmp.Or(cmp.Compare(phaseOrder[a.phase], phaseOrder[b.phase]), cmp.Compare(a.wave, b.wave))
	})
	for _, s := range steps {
		slices.SortStableFunc(s.objs, func(a, b *unstructured.Unstructured) int {
			return cmp.Or(cmp.Compare(order(a.GetKind()), order(b.GetKind())), cmp.Compare(a.GetName(), b.GetName()))
		})
	}
	return steps, failHooks
}
