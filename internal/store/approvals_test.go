package store

import (
	"context"
	"errors"
	"testing"

	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

// blockedDeploy creates a production deploy by author and parks it at the
// approval gate, the way PromoteReadyJobs does.
func blockedDeploy(t *testing.T, st *Store, repo, author string) int64 {
	t.Helper()
	ctx := context.Background()
	_, jobID := makeEnvPipeline(t, st, repo, "main", "sha-approve", "production", author)
	if _, err := st.PromoteReadyJobs(ctx); err != nil {
		t.Fatalf("PromoteReadyJobs: %v", err)
	}
	j, err := st.GetJob(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != "blocked" {
		t.Fatalf("deploy status = %q, want blocked", j.Status)
	}
	return jobID
}

func protect(t *testing.T, st *Store, repo string, roles []string, allowSelf bool) {
	t.Helper()
	if _, err := st.UpsertProtectedEnv(context.Background(), proto.ProtectedEnvironment{
		Repo: repo, Name: "production", RequiredApprovals: 1, ApprovalTimeoutHours: 24,
		ApproverRoles: roles, AllowSelfApproval: allowSelf,
	}); err != nil {
		t.Fatalf("UpsertProtectedEnv: %v", err)
	}
}

func vote(st *Store, jobID int64, who Approver) error {
	_, err := st.Approve(context.Background(), jobID, who, proto.ApprovalRequest{Verdict: "approved"})
	return err
}

// A developer may not approve when approver_roles leaves developers out; an
// owner may, and the vote releases the job.
func TestApproveEnforcesApproverRoles(t *testing.T) {
	ctx := context.Background()
	st := newDeployTestStore(t)
	const repo = "acme/approve-roles"
	protect(t, st, repo, []string{"admin", "owner"}, true)
	for user, role := range map[string]string{"dev@acme.io": "developer", "Owner@Acme.io": "owner"} {
		if _, err := st.AddMember(ctx, repo, user, role); err != nil {
			t.Fatal(err)
		}
	}
	jobID := blockedDeploy(t, st, repo, "someone@acme.io")

	if err := vote(st, jobID, Approver{Name: "dev@acme.io"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("developer vote: err = %v, want ErrForbidden", err)
	}
	if _, err := st.CanApprove(ctx, jobID, Approver{Name: "dev@acme.io"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("CanApprove(developer) = %v, want ErrForbidden", err)
	}
	// Rejecting is a vote too: a developer can't fail the deploy either.
	if _, err := st.Approve(ctx, jobID, Approver{Name: "dev@acme.io"},
		proto.ApprovalRequest{Verdict: "rejected"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("developer reject: err = %v, want ErrForbidden", err)
	}

	// Member usernames match the session email case-insensitively.
	if _, err := st.CanApprove(ctx, jobID, Approver{Name: "owner@acme.io"}); err != nil {
		t.Fatalf("CanApprove(owner) = %v, want nil", err)
	}
	if err := vote(st, jobID, Approver{Name: "owner@acme.io"}); err != nil {
		t.Fatalf("owner vote: %v", err)
	}
	j, _ := st.GetJob(ctx, jobID)
	if j.Status != "pending" {
		t.Fatalf("after owner approval status = %q, want pending", j.Status)
	}

	// When the rule does list developers, a developer may approve.
	protect(t, st, repo, []string{"developer"}, true)
	jobID = blockedDeploy(t, st, repo, "someone@acme.io")
	if err := vote(st, jobID, Approver{Name: "dev@acme.io"}); err != nil {
		t.Fatalf("developer vote with developer in approver_roles: %v", err)
	}
}

// The root cause of the bypass: a repo with no members skipped the role check
// entirely, so anyone could approve whatever approver_roles said. Now nobody
// but a platform admin (when admin is an approver role) may.
func TestApproveRepoWithoutMembersIsNotOpen(t *testing.T) {
	st := newDeployTestStore(t)
	const repo = "acme/no-members"
	jobID := blockedDeploy(t, st, repo, "author@acme.io") // global rule: admin, owner

	if err := vote(st, jobID, Approver{Name: "anyone@acme.io"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-member vote on memberless repo: err = %v, want ErrForbidden", err)
	}
	// An ADMIN_EMAILS identity typed as a plain name is not an admin.
	if err := vote(st, jobID, Approver{Name: "admin@acme.io"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("asserted admin vote: err = %v, want ErrForbidden", err)
	}
	if err := vote(st, jobID, Approver{Name: "admin@acme.io", PlatformAdmin: true}); err != nil {
		t.Fatalf("platform admin vote: %v", err)
	}

	// A platform admin is only "admin": a rule for owners alone excludes them.
	protect(t, st, repo, []string{"owner"}, true)
	jobID = blockedDeploy(t, st, repo, "author@acme.io")
	if err := vote(st, jobID, Approver{Name: "admin@acme.io", PlatformAdmin: true}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("platform admin vote on owner-only rule: err = %v, want ErrForbidden", err)
	}
}

// Self-approval is refused unless the rule allows it, whatever the case of the
// author's identity.
func TestApproveSelfApproval(t *testing.T) {
	ctx := context.Background()
	st := newDeployTestStore(t)
	const repo = "acme/self-approve"
	if _, err := st.AddMember(ctx, repo, "lead@acme.io", "owner"); err != nil {
		t.Fatal(err)
	}
	protect(t, st, repo, []string{"owner"}, false)
	jobID := blockedDeploy(t, st, repo, "Lead@Acme.io")
	if err := vote(st, jobID, Approver{Name: "lead@acme.io"}); !errors.Is(err, ErrSelfApproval) {
		t.Fatalf("self vote: err = %v, want ErrSelfApproval", err)
	}

	protect(t, st, repo, []string{"owner"}, true)
	if err := vote(st, jobID, Approver{Name: "lead@acme.io"}); err != nil {
		t.Fatalf("self vote with allow_self_approval: %v", err)
	}
}

// A manual job parked in 'blocked' waits for Play, not votes; approving it
// would release it to a runner past its needs and past Play.
func TestApproveRefusesManualJob(t *testing.T) {
	ctx := context.Background()
	st := newDeployTestStore(t)
	const repo = "acme/manual-prod"
	if _, err := st.AddMember(ctx, repo, "owner@acme.io", "owner"); err != nil {
		t.Fatal(err)
	}
	_, jobID := makeEnvPipeline(t, st, repo, "main", "sha-m", "production", "a@acme.io")
	if _, err := st.pool.Exec(ctx, `UPDATE jobs SET status='blocked', manual=TRUE WHERE id=$1`, jobID); err != nil {
		t.Fatal(err)
	}
	if err := vote(st, jobID, Approver{Name: "owner@acme.io"}); !errors.Is(err, ErrNotBlocked) {
		t.Fatalf("vote on manual job: err = %v, want ErrNotBlocked", err)
	}
}

// A rule with no roles (every box unticked, or a row written before the
// default applied) means the default admin/owner, never developers and never
// "nobody".
func TestApproveEmptyRolesMeansDefault(t *testing.T) {
	ctx := context.Background()
	st := newDeployTestStore(t)
	const repo = "acme/empty-roles"
	for user, role := range map[string]string{"dev@acme.io": "developer", "owner@acme.io": "owner"} {
		if _, err := st.AddMember(ctx, repo, user, role); err != nil {
			t.Fatal(err)
		}
	}
	protect(t, st, repo, []string{}, true)
	if _, err := st.pool.Exec(ctx,
		`UPDATE protected_environments SET approver_roles='{}' WHERE repo=$1`, repo); err != nil {
		t.Fatal(err)
	}
	jobID := blockedDeploy(t, st, repo, "someone@acme.io")
	if err := vote(st, jobID, Approver{Name: "dev@acme.io"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("developer vote under empty roles: err = %v, want ErrForbidden", err)
	}
	if err := vote(st, jobID, Approver{Name: "owner@acme.io"}); err != nil {
		t.Fatalf("owner vote under empty roles: %v", err)
	}
}
