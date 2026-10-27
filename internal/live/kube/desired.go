package kube

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/priytamjeepandey/forge-ci/internal/live"
)

// Desired state: each application's directory in the source, rendered at
// the branch tip, compared with the live top-level objects. The comparison
// runs where both meet — when a live object changes, and when the desired
// state does — so drift shows the moment either side moves.

// root is a top-level object of an application, as last seen live.
type root struct {
	uid   string
	app   *live.App
	live  []byte // trimmed manifest JSON
	forge bool   // Forge applied it
	hook  bool   // a sync hook: run by syncs, never compared or pruned
}

// appDesired is an application's desired state at one revision.
type appDesired struct {
	rev     live.Revision
	objs    map[string]*unstructured.Unstructured // by live.Key; watched kinds only
	missing map[string]bool                       // keys shown as Missing placeholders
}

// watchedKinds are group/kind pairs the provider watches: only those can be
// compared (a desired ServiceAccount is applied on sync but not shown).
var watchedKinds = func() map[string]bool {
	out := map[string]bool{}
	for _, k := range kinds {
		out[k.group+"/"+k.name] = true
	}
	return out
}()

func keyOf(u *unstructured.Unstructured) string {
	gv := u.GetAPIVersion()
	group := ""
	if i := strings.Index(gv, "/"); i >= 0 {
		group = gv[:i]
	}
	return live.Key(group, u.GetKind(), u.GetNamespace(), u.GetName())
}

// syncOf compares a live root with its desired manifest: "" when the app has
// no desired state, the root is a hook, or it is not in Git and Forge did
// not put it there (someone else manages it). Not in Git but applied by
// Forge, it is OutOfSync and requires pruning.
func (d *appDesired) syncOf(key string, rt *root) (sync string, prune bool) {
	if d == nil || rt.hook {
		return "", false
	}
	obj := d.objs[key]
	if obj == nil {
		if rt.forge {
			return live.OutOfSync, true
		}
		return "", false
	}
	var lv map[string]any
	if json.Unmarshal(rt.live, &lv) != nil {
		return live.OutOfSync, false
	}
	if ok, _ := inSync(obj.Object, lv); ok {
		return live.Synced, false
	}
	return live.OutOfSync, false
}

// noteRoot records a live top-level object and sets its sync status (w.mu held).
func (w *watcher) noteRoot(n *node, res *live.Resource) {
	if w.p.repo == nil || n.owner != "" || n.app == nil || n.app.Orphans {
		return
	}
	key := res.Key()
	n.key = key
	rt := &root{uid: res.UID, app: n.app, live: res.Manifest, forge: n.forge, hook: res.Hook != ""}
	w.roots[key] = rt
	if w.rootsByApp[n.app.Key] == nil {
		w.rootsByApp[n.app.Key] = map[string]bool{}
	}
	w.rootsByApp[n.app.Key][key] = true
	d := w.desired[n.app.Key]
	res.Sync, res.RequiresPruning = d.syncOf(key, rt)
	if d != nil && d.missing[key] {
		delete(d.missing, key)
		w.sink.Delete(*n.app, live.DesiredPrefix+key)
	}
}

// forgetRoot drops a removed top-level object; if it is desired it shows as
// Missing (w.mu held).
func (w *watcher) forgetRoot(n *node) {
	if n.key == "" || n.app == nil {
		return
	}
	delete(w.roots, n.key)
	delete(w.rootsByApp[n.app.Key], n.key)
	if d := w.desired[n.app.Key]; d != nil && d.objs[n.key] != nil {
		d.missing[n.key] = true
		w.sink.Upsert(*n.app, placeholder(d.objs[n.key]))
	}
}

func placeholder(u *unstructured.Unstructured) live.Resource {
	gv := u.GetAPIVersion()
	group, version := "", gv
	if i := strings.Index(gv, "/"); i >= 0 {
		group, version = gv[:i], gv[i+1:]
	}
	return live.Resource{Group: group, Version: version, Kind: u.GetKind(), Namespace: u.GetNamespace(), Name: u.GetName(),
		UID: live.DesiredPrefix + keyOf(u), Health: &live.Health{Status: live.Missing, Message: "In Git, not in the cluster"}, Sync: live.OutOfSync}
}

// setDesired installs an app's desired state: every root gets its sync
// status, desired objects with no live counterpart show as Missing, and the
// manifests go to Forge for the Diff view.
func (w *watcher) setDesired(app live.App, rev live.Revision, path string, r *rendered, renderErr error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	a := w.apps[app.Key]
	if a == nil {
		return
	}
	prev := w.desired[app.Key]
	out := live.Desired{Revision: rev, Source: path, Manifests: map[string]json.RawMessage{}}
	if renderErr != nil {
		out.Error = renderErr.Error()
		w.desired[app.Key] = nil
	} else {
		out.Tool = r.tool
		d := &appDesired{rev: rev, objs: map[string]*unstructured.Unstructured{}, missing: map[string]bool{}}
		for _, u := range r.objects {
			if hookOf(u.GetAnnotations()).phase != "" {
				continue // hooks run during a sync; they are not desired state
			}
			key := keyOf(u)
			out.Manifests[key] = manifestJSON(u)
			if watchedKinds[strings.SplitN(key, "/", 3)[0]+"/"+u.GetKind()] {
				d.objs[key] = u
			}
		}
		w.desired[app.Key] = d
	}
	d := w.desired[app.Key]
	for key := range w.rootsByApp[app.Key] {
		rt := w.roots[key]
		sync, prune := d.syncOf(key, rt)
		w.sink.SetSync(*a, rt.uid, sync, prune)
	}
	if prev != nil {
		for key := range prev.missing {
			if d == nil || d.objs[key] == nil || w.roots[key] != nil {
				w.sink.Delete(*a, live.DesiredPrefix+key)
			}
		}
	}
	if d != nil {
		for key, u := range d.objs {
			if w.roots[key] == nil {
				d.missing[key] = true
				w.sink.Upsert(*a, placeholder(u))
			}
		}
	}
	w.sink.Desired(*a, out)
}

// refreshReq asks the follow loop to read the source now.
type refreshReq struct {
	hard bool
	done chan struct{}
}

// follow keeps desired state current: it fetches the branch every poll (or
// on a refresh) and re-renders the apps whose directory the new commits
// touched, plus any app that has none yet.
func (w *watcher) follow(ctx context.Context) {
	poll := w.p.repo.src.Poll
	if poll <= 0 {
		poll = 30 * time.Second
	}
	last := ""
	var pending []refreshReq
	for {
		hard := false
		for _, r := range pending {
			hard = hard || r.hard
		}
		if err := w.sync(ctx, &last, hard); err != nil && ctx.Err() == nil {
			slog.Warn("live: desired state", "err", err)
		}
		for _, r := range pending {
			close(r.done)
		}
		pending = nil
		select {
		case <-ctx.Done():
			return
		case r := <-w.refresh:
			pending = append(pending, r)
		case <-time.After(poll):
		}
	}
}

// sync is one pass of follow.
func (w *watcher) sync(ctx context.Context, last *string, hard bool) error {
	repo := w.p.repo
	sha, err := repo.fetch(ctx)
	if err != nil {
		return err
	}
	var changed []string
	all := hard || *last == ""
	if !all && sha != *last {
		if changed, err = repo.changed(ctx, *last, sha); err != nil {
			all = true
		}
	}
	rev, err := repo.revision(ctx, sha)
	if err != nil {
		return err
	}
	w.mu.Lock()
	w.head = sha
	var todo []live.App
	for key, keys := range w.rootsByApp {
		a := w.apps[key]
		if a == nil || len(keys) == 0 {
			continue
		}
		path := repo.appPath(*a)
		_, known := w.desired[key]
		if all || !known || touched(changed, path) {
			todo = append(todo, *a)
		}
	}
	w.mu.Unlock()
	for _, a := range todo {
		path := repo.appPath(a)
		r, err := repo.render(ctx, sha, path, a.Namespace, a.Name)
		w.setDesired(a, rev, path, r, err)
	}
	*last = sha
	return nil
}

func touched(changed []string, dir string) bool {
	for _, p := range changed {
		if p == dir || strings.HasPrefix(p, dir+"/") {
			return true
		}
	}
	return false
}
