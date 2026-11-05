package store

import (
	"context"
	"errors"
	"testing"

	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

const retryOnceYAML = `
stages: [test]
jobs:
  unit:
    stage: test
    retry: 1
    script: [make test]
`

// TestCompleteJobClaimIsIdempotent: a completion report scoped to a claim
// lands once; a resend of it (response lost) gets ErrNotFound and cannot
// complete the next attempt that another runner has claimed.
func TestCompleteJobClaimIsIdempotent(t *testing.T) {
	st := newArtifactsTestStore(t)
	ctx := context.Background()
	createPipelineT(t, st, "acme/claims", "main", retryOnceYAML)
	if _, err := st.PromoteReadyJobs(ctx); err != nil {
		t.Fatal(err)
	}

	first, err := st.AcquireJob(ctx, proto.AcquireRequest{RunnerID: "r1"})
	if err != nil || first == nil {
		t.Fatalf("acquire: %v %v", first, err)
	}
	if first.Attempt != 1 || first.RunnerID != "r1" {
		t.Fatalf("claim = attempt %d runner %q, want 1/r1", first.Attempt, first.RunnerID)
	}
	final, _, err := st.CompleteJobClaim(ctx, first.ID, first.Attempt, first.RunnerID, "failed", 1)
	if err != nil || final != "pending" {
		t.Fatalf("first report: final=%q err=%v, want pending (retry)", final, err)
	}

	second, err := st.AcquireJob(ctx, proto.AcquireRequest{RunnerID: "r2"})
	if err != nil || second == nil || second.ID != first.ID || second.Attempt != 2 {
		t.Fatalf("re-acquire: %+v %v", second, err)
	}
	// r1's resent report must not touch r2's attempt.
	if _, _, err := st.CompleteJobClaim(ctx, first.ID, first.Attempt, first.RunnerID, "failed", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("resent report: err=%v, want ErrNotFound", err)
	}
	if _, _, err := st.CompleteJobClaim(ctx, first.ID, 2, "r1", "failed", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("report from the wrong runner: err=%v, want ErrNotFound", err)
	}
	final, _, err = st.CompleteJobClaim(ctx, second.ID, second.Attempt, second.RunnerID, "success", 0)
	if err != nil || final != "success" {
		t.Fatalf("r2 report: final=%q err=%v", final, err)
	}
	if _, _, err := st.CompleteJobClaim(ctx, second.ID, second.Attempt, second.RunnerID, "success", 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("repeat of a recorded report: err=%v, want ErrNotFound", err)
	}
}

// TestAcceptLogChunkDropsResends: a chunk number at or below the last one
// accepted is a resend; a new claim starts numbering again.
func TestAcceptLogChunkDropsResends(t *testing.T) {
	st := newArtifactsTestStore(t)
	ctx := context.Background()
	createPipelineT(t, st, "acme/logseq", "main", retryOnceYAML)
	if _, err := st.PromoteReadyJobs(ctx); err != nil {
		t.Fatal(err)
	}
	j, err := st.AcquireJob(ctx, proto.AcquireRequest{RunnerID: "r1"})
	if err != nil || j == nil {
		t.Fatalf("acquire: %v %v", j, err)
	}
	for _, c := range []struct {
		seq  int64
		want bool
	}{{1, true}, {1, false}, {2, true}, {1, false}, {3, true}} {
		got, err := st.AcceptLogChunk(ctx, j.ID, c.seq)
		if err != nil || got != c.want {
			t.Fatalf("seq %d: got %v err %v, want %v", c.seq, got, err, c.want)
		}
	}
	if _, _, err := st.CompleteJobClaim(ctx, j.ID, j.Attempt, j.RunnerID, "failed", 1); err != nil {
		t.Fatal(err)
	}
	j2, err := st.AcquireJob(ctx, proto.AcquireRequest{RunnerID: "r1"})
	if err != nil || j2 == nil {
		t.Fatalf("re-acquire: %v %v", j2, err)
	}
	if ok, _ := st.AcceptLogChunk(ctx, j2.ID, 1); !ok {
		t.Fatal("the retried attempt's first chunk was taken for a resend")
	}
}
