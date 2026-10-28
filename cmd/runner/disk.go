package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Disk janitor: builds on a long-lived machine leave images, BuildKit cache
// and cache volumes behind, and with nothing bounding them the disk fills
// until every job fails with "no space left on device". Before and after
// each job the runner tidies up: always the cheap leftovers (stopped
// containers, dangling images, anonymous volumes, stale job dirs) and the
// build cache down to RUNNER_BUILD_CACHE_MAX; then, while the Docker disk
// has less than RUNNER_DISK_MIN_FREE free, it escalates — older images, the
// older build cache, unused volumes (package caches), all unused build cache
// — and stops as soon as there is room. Nothing a running job uses is
// removed: Docker refuses to remove in-use images, volumes and cache, and
// images younger than an hour (built, about to be pushed) are kept.
// RUNNER_DISK_JANITOR=off opts out.

const (
	defaultDiskMinFree   = 30 // percent of the disk
	defaultBuildCacheMax = "20GB"
	janitorEvery         = time.Minute // the routine tidy, at most this often
	staleJobDirAfter     = 12 * time.Hour
	janitorStepTimeout   = 5 * time.Minute
)

// janitor is this runner's disk janitor; nil when off (sweep is then a no-op).
var janitor *diskJanitor

type diskJanitor struct {
	mu       sync.Mutex
	lastTidy time.Time
	minFree  float64 // percent
	cacheMax string
	root     string // filesystem to watch: Docker's data root
	tmp      string // where job dirs live
	// docker runs one docker CLI command; free reports the free and total
	// bytes of a filesystem. Replaced in tests.
	docker func(ctx context.Context, args ...string) (string, error)
	free   func(path string) (avail, total uint64, err error)
	now    func() time.Time
}

// newDiskJanitor returns nil when it is off or Docker is not usable here.
func newDiskJanitor() *diskJanitor {
	if strings.EqualFold(os.Getenv("RUNNER_DISK_JANITOR"), "off") {
		slog.Info("disk janitor: off (RUNNER_DISK_JANITOR=off)")
		return nil
	}
	if _, err := exec.LookPath("docker"); err != nil {
		return nil
	}
	j := &diskJanitor{
		minFree:  float64(defaultDiskMinFree),
		cacheMax: envOr("RUNNER_BUILD_CACHE_MAX", defaultBuildCacheMax),
		tmp:      os.TempDir(),
		docker:   runDocker,
		free:     diskFree,
		now:      time.Now,
	}
	if v, err := strconv.ParseFloat(strings.TrimSuffix(os.Getenv("RUNNER_DISK_MIN_FREE"), "%"), 64); err == nil && v > 0 && v < 100 {
		j.minFree = v
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	root, err := j.docker(ctx, "info", "--format", "{{.DockerRootDir}}")
	if err != nil || strings.TrimSpace(root) == "" {
		slog.Info("disk janitor: docker not reachable; not tidying", "err", err)
		return nil
	}
	j.root = strings.TrimSpace(root)
	if _, _, err := j.free(j.root); err != nil {
		slog.Info("disk janitor: cannot read free space; not tidying", "path", j.root, "err", err)
		return nil
	}
	slog.Info("disk janitor: on", "docker_root", j.root, "min_free_pct", j.minFree, "build_cache_max", j.cacheMax)
	return j
}

func runDocker(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	return string(out), err
}

// freePct is the free share of the Docker disk, in percent.
func (j *diskJanitor) freePct() (pct float64, avail uint64, ok bool) {
	a, t, err := j.free(j.root)
	if err != nil || t == 0 {
		return 0, 0, false
	}
	return float64(a) * 100 / float64(t), a, true
}

// sweep tidies the disk; when says at what point ("before the job"). It returns
// one line for the job log, empty when there was nothing to say. Concurrent
// jobs share one sweep: whoever finds it running skips rather than waits.
func (j *diskJanitor) sweep(ctx context.Context, when string) string {
	if j == nil || !j.mu.TryLock() {
		return ""
	}
	defer j.mu.Unlock()
	before, availBefore, ok := j.freePct()
	if !ok {
		return ""
	}
	low := before < j.minFree
	if !low && j.now().Sub(j.lastTidy) < janitorEvery {
		return ""
	}
	j.lastTidy = j.now()

	// Always: leftovers no job will use again, and the build cache bounded.
	j.removeStaleJobDirs()
	j.run(ctx, "container", "prune", "-f", "--filter", "until=1h")
	j.run(ctx, "image", "prune", "-f")
	j.run(ctx, "volume", "prune", "-f")
	if !j.run(ctx, "builder", "prune", "-f", "--max-used-space", j.cacheMax) {
		j.run(ctx, "builder", "prune", "-f", "--keep-storage", j.cacheMax) // Docker before 27
	}

	// Low on space: escalate, cheapest-to-rebuild first, until there's room.
	steps := [][]string{
		{"image", "prune", "-af", "--filter", "until=1h"},
		{"builder", "prune", "-af", "--filter", "until=1h"},
		{"volume", "prune", "-af"},
		{"builder", "prune", "-af"},
	}
	used := []string{}
	for _, s := range steps {
		pct, _, ok := j.freePct()
		if !ok || pct >= j.minFree {
			break
		}
		j.run(ctx, s...)
		used = append(used, strings.Join(s[:2], " "))
	}

	after, availAfter, _ := j.freePct()
	freed := int64(availAfter) - int64(availBefore)
	if !low && freed < 1<<30 {
		return "" // routine tidy, nothing worth a line
	}
	msg := fmt.Sprintf("disk: %.0f%% free (%s) %s", after, gib(availAfter), when)
	if freed > 0 {
		msg += fmt.Sprintf("; freed %s", gib(uint64(freed)))
	}
	if len(used) > 0 {
		msg += " (" + strings.Join(used, ", ") + ")"
	}
	if after < j.minFree {
		msg += fmt.Sprintf("; still below %.0f%% — builds may fail for lack of space", j.minFree)
	}
	slog.Info(msg)
	return msg + "\n"
}

func (j *diskJanitor) run(ctx context.Context, args ...string) bool {
	ctx, cancel := context.WithTimeout(ctx, janitorStepTimeout)
	defer cancel()
	if out, err := j.docker(ctx, args...); err != nil {
		slog.Warn("disk janitor: docker "+strings.Join(args, " ")+" failed", "err", err, "out", strings.TrimSpace(out))
		return false
	}
	return true
}

// removeStaleJobDirs deletes job and run dirs a crashed runner left behind;
// a live job is far younger than the longest job timeout.
func (j *diskJanitor) removeStaleJobDirs() {
	for _, pattern := range []string{"forge-job-*", "forge-run-*"} {
		dirs, _ := filepath.Glob(filepath.Join(j.tmp, pattern))
		for _, d := range dirs {
			if fi, err := os.Stat(d); err == nil && j.now().Sub(fi.ModTime()) > staleJobDirAfter {
				_ = os.RemoveAll(d)
			}
		}
	}
}

func gib(b uint64) string { return fmt.Sprintf("%.1f GB", float64(b)/(1<<30)) }
