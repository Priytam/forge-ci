// mailpreview prints the HTML of a sample notification email, so the
// template can be checked in real mail clients (paste into an HTML test
// sender, or open the file in a browser):
//
//	go run ./cmd/mailpreview [approval|failed|fixed|deploy] > sample.html
package main

import (
	"fmt"
	"os"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/notify"
	"github.com/priytamjeepandey/forge-ci/internal/store"
)

func main() {
	kind := "approval"
	if len(os.Args) > 1 {
		kind = os.Args[1]
	}
	const frontend = "https://forge.example.com"
	var ev notify.Event
	links := notify.ApprovalLinks{}
	switch kind {
	case "failed":
		ev = notify.PipelineEvent(store.PipelineEventNotice{Kind: notify.EventPipelineFailed, PipelineID: 812, Repo: "acme/shop", Ref: "main",
			SHA: "1a2b3c4d5e6f", Status: "failed", TriggeredBy: "harish@acme.io", CommitMessage: "Fix totals on the cart page",
			FailedJobs: []string{"unit", "lint"}, Duration: 7*time.Minute + 12*time.Second}, frontend)
	case "fixed":
		ev = notify.PipelineEvent(store.PipelineEventNotice{Kind: notify.EventPipelineFixed, PipelineID: 813, Repo: "acme/shop", Ref: "main",
			SHA: "9f8e7d6c5b4a", Status: "success", TriggeredBy: "harish@acme.io", CommitMessage: "Fix totals, take two", Duration: 6 * time.Minute}, frontend)
	case "deploy":
		ev = notify.DeployEvent(store.DeployEventNotice{Kind: notify.EventDeploySucceeded, JobID: 4410, JobName: "deploy-production", PipelineID: 813,
			Repo: "acme/shop", Ref: "main", SHA: "9f8e7d6c5b4a", Environment: "production", Status: "success", DeployedBy: "ravi@acme.io",
			URL: "https://shop.example.com", Version: "2.3.0", Digest: "sha256:9f86d081884c7d65"}, frontend)
	default:
		ev = notify.ApprovalEvent(store.ApprovalNotice{JobID: 4409, JobName: "deploy-production", PipelineID: 813, Repo: "acme/shop", Ref: "main",
			SHA: "9f8e7d6c5b4a", Environment: "production", TriggeredBy: "harish@acme.io", CommitMessage: "Fix totals, take two",
			RequiredApprovals: 2, ApproverRoles: []string{"owner", "admin"}, RequiredGroup: "release-approvers", RequiredGroupApprovals: 1,
			Approvers: []string{"ravi@acme.io", "ashwin@acme.io"}}, frontend)
		links = notify.ApprovalLinks{Approve: frontend + "/api/v1/approvals/link?t=example-approve", Reject: frontend + "/api/v1/approvals/link?t=example-reject"}
	}
	fmt.Print(notify.RenderHTML(ev, links))
}
