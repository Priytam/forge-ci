package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeDisk is a 100 GB disk whose free space each docker command can change.
type fakeDisk struct {
	free  uint64
	cmds  []string
	frees map[string]uint64 // command prefix → bytes it frees
}

func (f *fakeDisk) janitor(t *testing.T) *diskJanitor {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	return &diskJanitor{
		minFree: 30, cacheMax: "20GB", root: "/var/lib/docker", tmp: t.TempDir(),
		docker: func(_ context.Context, args ...string) (string, error) {
			c := strings.Join(args, " ")
			f.cmds = append(f.cmds, c)
			for prefix, n := range f.frees {
				if strings.HasPrefix(c, prefix) {
					f.free += n
				}
			}
			return "", nil
		},
		free: func(string) (uint64, uint64, error) { return f.free, 100 << 30, nil },
		now:  func() time.Time { return now },
	}
}

// TestJanitorEscalatesUntilThereIsRoom: a nearly full disk gets the routine
// tidy, then stronger steps only until 30% is free.
func TestJanitorEscalatesUntilThereIsRoom(t *testing.T) {
	f := &fakeDisk{free: 2 << 30, frees: map[string]uint64{
		"image prune -af":   10 << 30,
		"builder prune -af": 25 << 30, // the first builder step reaches 37 GB
		"volume prune -af":  40 << 30,
	}}
	line := f.janitor(t).sweep(context.Background(), "before the job")
	want := []string{
		"container prune -f --filter until=1h", "image prune -f", "volume prune -f",
		"builder prune -f --max-used-space 20GB",
		"image prune -af --filter until=1h", "builder prune -af --filter until=1h",
	}
	if strings.Join(f.cmds, "|") != strings.Join(want, "|") {
		t.Fatalf("commands:\n%s\nwant:\n%s", strings.Join(f.cmds, "\n"), strings.Join(want, "\n"))
	}
	if !strings.Contains(line, "37% free") || !strings.Contains(line, "freed 35.0 GB") || strings.Contains(line, "still below") {
		t.Fatalf("log line = %q", line)
	}
}

// TestJanitorRoutineTidyIsQuietAndThrottled: with room to spare only the
// routine tidy runs, says nothing, and not again within a minute.
func TestJanitorRoutineTidyIsQuietAndThrottled(t *testing.T) {
	f := &fakeDisk{free: 60 << 30}
	j := f.janitor(t)
	if line := j.sweep(context.Background(), "after the job"); line != "" {
		t.Fatalf("routine tidy logged %q", line)
	}
	if len(f.cmds) != 4 {
		t.Fatalf("routine tidy ran %v", f.cmds)
	}
	j.sweep(context.Background(), "before the job")
	if len(f.cmds) != 4 {
		t.Fatalf("tidied again within a minute: %v", f.cmds)
	}
}

// TestJanitorSaysWhenStillFull: everything tried and still short → the job
// log says so.
func TestJanitorSaysWhenStillFull(t *testing.T) {
	f := &fakeDisk{free: 1 << 30}
	line := f.janitor(t).sweep(context.Background(), "before the job")
	if len(f.cmds) != 8 || !strings.Contains(line, "still below 30%") {
		t.Fatalf("cmds=%v line=%q", f.cmds, line)
	}
}

// TestJanitorRemovesOnlyStaleJobDirs: a crashed runner's old job dir goes,
// a running job's stays.
func TestJanitorRemovesOnlyStaleJobDirs(t *testing.T) {
	f := &fakeDisk{free: 60 << 30}
	j := f.janitor(t)
	old := filepath.Join(j.tmp, "forge-job-old")
	live := filepath.Join(j.tmp, "forge-run-live")
	for _, d := range []string{old, live} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.Chtimes(old, j.now().Add(-13*time.Hour), j.now().Add(-13*time.Hour))
	_ = os.Chtimes(live, j.now().Add(-time.Hour), j.now().Add(-time.Hour))
	j.sweep(context.Background(), "after the job")
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("stale job dir was kept")
	}
	if _, err := os.Stat(live); err != nil {
		t.Error("a live job dir was removed")
	}
}

func TestJanitorNilIsNoop(t *testing.T) {
	var j *diskJanitor
	if j.sweep(context.Background(), "before the job") != "" {
		t.Fatal("nil janitor said something")
	}
}
