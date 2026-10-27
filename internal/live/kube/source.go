package kube

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	"github.com/priytamjeepandey/forge-ci/internal/live"
)

// Source is where applications' desired state lives: one Git repository,
// one directory per application. Rendering happens in the agent, next to
// the cluster, with the same tools people use by hand.
type Source struct {
	URL    string // clone URL (credentials, if any, in Token)
	Token  string
	Branch string // default "main"
	// Path names an application's directory: {group}, {name} and
	// {namespace} are the app's (default "apps/{group}/{name}").
	Path string
	// Poll is how often the branch is fetched (default 30s); a refresh
	// fetches at once.
	Poll time.Duration
	// Dir holds the clone (default a temp directory).
	Dir string
}

// repo is a bare clone of the source, fetched on demand.
type repo struct {
	src Source
	dir string

	// Source trees by revision (the last few), for rendering: an app's
	// directory may use files outside it (a Kustomize overlay's base).
	tmu   sync.Mutex
	trees map[string]string
	order []string
}

// keptTrees is how many extracted revisions a repo keeps.
const keptTrees = 3

func newRepo(src Source) (*repo, error) {
	if src.Branch == "" {
		src.Branch = "main"
	}
	if src.Path == "" {
		src.Path = "apps/{group}/{name}"
	}
	dir := src.Dir
	if dir == "" {
		d, err := os.MkdirTemp("", "forge-live-source-")
		if err != nil {
			return nil, err
		}
		dir = d
	}
	return &repo{src: src, dir: filepath.Join(dir, "repo.git")}, nil
}

// git runs git in the clone with the source's token as an HTTP header (never
// in a URL, so it never lands in an error message or the clone's config).
func (r *repo) git(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	full := []string{"-c", "core.askPass=true"}
	if r.src.Token != "" {
		full = append(full, "-c", "http.extraHeader=Authorization: Bearer "+r.src.Token)
	}
	cmd := exec.CommandContext(ctx, "git", append(full, args...)...)
	cmd.Dir = r.dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	cmd.Stdin = stdin
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// fetch brings the branch up to date and returns its tip.
func (r *repo) fetch(ctx context.Context) (string, error) {
	if _, err := os.Stat(r.dir); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(r.dir, 0o755); err != nil {
			return "", err
		}
		if _, err := r.git(ctx, nil, "init", "--bare", "-q"); err != nil {
			return "", err
		}
	}
	ref := "refs/heads/" + r.src.Branch
	if _, err := r.git(ctx, nil, "fetch", "-q", "--prune", r.src.URL, "+"+ref+":"+ref); err != nil {
		return "", err
	}
	out, err := r.git(ctx, nil, "rev-parse", ref)
	return strings.TrimSpace(string(out)), err
}

// revision describes a commit.
func (r *repo) revision(ctx context.Context, sha string) (live.Revision, error) {
	out, err := r.git(ctx, nil, "log", "-1", "--format=%H%x00%an%x00%aI%x00%s", sha)
	if err != nil {
		return live.Revision{}, err
	}
	f := strings.SplitN(strings.TrimSpace(string(out)), "\x00", 4)
	if len(f) != 4 {
		return live.Revision{}, fmt.Errorf("git log %s: unexpected output", sha)
	}
	t, _ := time.Parse(time.RFC3339, f[2])
	return live.Revision{SHA: f[0], Author: f[1], Time: t, Message: f[3]}, nil
}

// changed lists the paths that differ between two commits.
func (r *repo) changed(ctx context.Context, from, to string) ([]string, error) {
	out, err := r.git(ctx, nil, "diff", "--name-only", from, to)
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(out)), nil
}

// appPath is an application's directory in the source.
func (r *repo) appPath(a live.App) string {
	return strings.NewReplacer("{group}", a.Group, "{name}", a.Name, "{namespace}", a.Namespace).Replace(r.src.Path)
}

// rendered is an application's desired objects at one revision.
type rendered struct {
	tool    string
	objects []*unstructured.Unstructured
}

// tree extracts the whole source at sha, once (r.tmu held).
func (r *repo) tree(ctx context.Context, sha string) (string, error) {
	if d, ok := r.trees[sha]; ok {
		return d, nil
	}
	archive, err := r.git(ctx, nil, "archive", "--format=tar", sha)
	if err != nil {
		return "", err
	}
	d, err := os.MkdirTemp(filepath.Dir(r.dir), "tree-")
	if err != nil {
		return "", err
	}
	if err := untar(bytes.NewReader(archive), d); err != nil {
		os.RemoveAll(d)
		return "", err
	}
	if r.trees == nil {
		r.trees = map[string]string{}
	}
	r.trees[sha], r.order = d, append(r.order, sha)
	for len(r.order) > keptTrees {
		os.RemoveAll(r.trees[r.order[0]])
		delete(r.trees, r.order[0])
		r.order = r.order[1:]
	}
	return d, nil
}

// render produces path's manifests at sha: `kustomize build` when there is
// a kustomization, `helm template` for a chart, otherwise the YAML files as
// they are. Objects without a namespace get the app's.
func (r *repo) render(ctx context.Context, sha, path, namespace, release string) (*rendered, error) {
	// Renders share the extracted trees: one at a time, so a tree is never
	// dropped under a render.
	r.tmu.Lock()
	defer r.tmu.Unlock()
	tree, err := r.tree(ctx, sha)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(tree, filepath.FromSlash(path))
	if _, err := os.Stat(dir); err != nil {
		return nil, fmt.Errorf("%s is not in the source at %s", path, shortSHA(sha))
	}
	var out []byte
	tool := "plain"
	switch {
	case exists(dir, "kustomization.yaml", "kustomization.yml", "Kustomization"):
		tool = "kustomize"
		if _, lookErr := exec.LookPath("kustomize"); lookErr == nil {
			out, err = run(ctx, "kustomize", "build", dir)
		} else {
			out, err = run(ctx, "kubectl", "kustomize", dir)
		}
	case exists(dir, "Chart.yaml"):
		tool = "helm"
		out, err = run(ctx, "helm", "template", release, dir, "--namespace", namespace)
	default:
		out, err = plainFiles(dir)
	}
	if err != nil {
		return nil, err
	}
	objs, err := decodeAll(out)
	if err != nil {
		return nil, err
	}
	for _, o := range objs {
		if o.GetNamespace() == "" && !clusterScoped[o.GetKind()] {
			o.SetNamespace(namespace)
		}
	}
	return &rendered{tool: tool, objects: objs}, nil
}

// clusterScoped kinds never get the app's namespace.
var clusterScoped = map[string]bool{
	"Namespace": true, "ClusterRole": true, "ClusterRoleBinding": true, "CustomResourceDefinition": true,
	"PersistentVolume": true, "StorageClass": true, "PriorityClass": true, "IngressClass": true,
}

func exists(dir string, names ...string) bool {
	for _, n := range names {
		if _, err := os.Stat(filepath.Join(dir, n)); err == nil {
			return true
		}
	}
	return false
}

func run(ctx context.Context, name string, args ...string) ([]byte, error) {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s: %v: %s", name, err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// plainFiles concatenates the directory's manifests, in name order.
func plainFiles(dir string) ([]byte, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if ext := filepath.Ext(e.Name()); !e.IsDir() && (ext == ".yaml" || ext == ".yml" || ext == ".json") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	var buf bytes.Buffer
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			return nil, err
		}
		buf.Write(b)
		buf.WriteString("\n---\n")
	}
	return buf.Bytes(), nil
}

// decodeAll reads a multi-document YAML (or JSON) stream into objects,
// unrolling Lists.
func decodeAll(b []byte) ([]*unstructured.Unstructured, error) {
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(b), 4096)
	var out []*unstructured.Unstructured
	for {
		var m map[string]any
		if err := dec.Decode(&m); errors.Is(err, io.EOF) {
			return out, nil
		} else if err != nil {
			return nil, err
		}
		if len(m) == 0 {
			continue
		}
		u := &unstructured.Unstructured{Object: m}
		if u.IsList() {
			if err := u.EachListItem(func(o runtime.Object) error {
				out = append(out, o.(*unstructured.Unstructured))
				return nil
			}); err != nil {
				return nil, err
			}
			continue
		}
		if u.GetKind() == "" || u.GetName() == "" {
			return nil, fmt.Errorf("a manifest has no kind or name")
		}
		out = append(out, u)
	}
}

// untar extracts a tar stream under dir, refusing paths that leave it.
func untar(r io.Reader, dir string) error {
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		target := filepath.Join(dir, filepath.FromSlash(h.Name))
		if !strings.HasPrefix(target, filepath.Clean(dir)+string(os.PathSeparator)) {
			return fmt.Errorf("archive path %q leaves the work directory", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, io.LimitReader(tr, 64<<20))
			f.Close()
			if err != nil {
				return err
			}
		}
	}
}

// manifestJSON is an object as the desired manifest Forge stores.
func manifestJSON(u *unstructured.Unstructured) json.RawMessage {
	b, _ := json.Marshal(u.Object)
	return b
}
