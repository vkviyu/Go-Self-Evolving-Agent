package app

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/policy"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacecoord"
)

type (
	// recordingApprover records the request it answers with approved.
	recordingApprover struct {
		approved bool
		request  policy.Request
		decision policy.Decision
	}
)

// Approve records request and decision.
func (approver *recordingApprover) Approve(_ context.Context, request policy.Request, decision policy.Decision) (bool, error) {
	approver.request, approver.decision = request, decision
	return approver.approved, nil
}

func TestOpenWiresSnapshotStoreWithLeaseBarrier(t *testing.T) {
	data, workspace := t.TempDir(), t.TempDir()
	application, err := testOpen(t, context.Background(), config.Config{
		HomeDirectory: t.TempDir(), DataDirectory: data, WorkspaceDirectory: workspace, CoordinationDirectory: t.TempDir(),
		Agents: config.AgentsConfig{SnapshotCopyLimit: 4096},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.Close() })
	snapshots := application.coordinator.Snapshots
	if snapshots == nil || snapshots.Barrier == nil || snapshots.CopyLimit != 4096 || snapshots.WorkspaceRoot != application.workspace.Root() {
		t.Fatalf("snapshot store = %#v", snapshots)
	}
	ctx := context.Background()
	root, _, err := application.EnsureRootThread(ctx, "barrier-session")
	if err != nil {
		t.Fatal(err)
	}
	release, err := snapshots.Barrier.Hold(ctx, root.TeamID, root.AgentID, "")
	if err != nil {
		t.Fatal(err)
	}
	// A shared writer cannot write while the barrier holds the workspace.
	write := orchestration.LeaseSpec{TeamID: root.TeamID, AgentID: root.AgentID, Path: "main.go", Mode: orchestration.LeaseWrite}
	if _, err := application.domain.Leases().AcquireLease(ctx, write); !errors.Is(err, orchestration.ErrLeaseConflict) {
		t.Fatalf("write beside the barrier: %v", err)
	}
	release()
	if _, err := application.domain.Leases().AcquireLease(ctx, write); err != nil {
		t.Fatalf("write after the barrier: %v", err)
	}
}

func TestMergeApproverAsksWithThePlanDiff(t *testing.T) {
	approver := &recordingApprover{approved: true}
	merge := workspacecoord.MergeRequest{ID: "merge-1", TeamID: "team-1", AgentID: "agent-1", TaskID: "task-1"}
	plan := workspacecoord.ApplyPlan{MergeID: merge.ID, CandidateDigest: "candidate", Digest: "plan", Entries: []workspacecoord.PlanEntry{{Path: "a.txt"}}}
	approved, err := mergeApprover(approver, nil).ApproveMerge(context.Background(), workspacecoord.MergeApproval{Merge: merge, Plan: plan, Diff: "+changed"})
	if err != nil || !approved {
		t.Fatalf("approved=%t err=%v", approved, err)
	}
	request := approver.request
	if request.ToolName != workspacecoord.MergeApplyToolName || request.Actor.AgentID != merge.AgentID || request.Actor.TaskID != merge.TaskID || approver.decision.Effect != policy.EffectRequireApproval {
		t.Fatalf("approval request = %#v, decision = %#v", request, approver.decision)
	}
	var arguments mergeApprovalArguments
	if err := json.Unmarshal(request.NormalizedArg, &arguments); err != nil {
		t.Fatal(err)
	}
	if arguments.PlanDigest != "plan" || arguments.Diff.Content != "+changed" || len(arguments.Paths) != 1 || arguments.Paths[0] != "a.txt" {
		t.Fatalf("approval arguments = %#v", arguments)
	}
	if mergeApprover(nil, nil) != nil {
		t.Fatal("merge approver without a user approver")
	}
}
