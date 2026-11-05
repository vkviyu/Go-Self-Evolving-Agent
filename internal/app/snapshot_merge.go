package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/policy"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacecoord"
)

const (
	// snapshotBarrierWait outlasts the longest shell operation (five minutes),
	// so a snapshot or merge waits out in-flight shared writers.
	snapshotBarrierWait = 6 * time.Minute
	// mergeApplyRuleID attributes snapshot merge approvals in the audit trail.
	mergeApplyRuleID = "merge.apply"
)

type (
	// agentLookup loads the Agent behind a merge for approval attribution.
	agentLookup interface {
		GetAgent(context.Context, orchestration.AgentID) (orchestration.AgentSnapshot, error)
	}

	// mergeApprovalArguments is the approval card payload. Diff uses the
	// shape of tool diff results, so the TUI renders it as a diff.
	mergeApprovalArguments struct {
		MergeID         orchestration.MergeID `json:"merge_id"`
		TaskID          orchestration.TaskID  `json:"task_id,omitempty"`
		CandidateDigest string                `json:"candidate_digest"`
		PlanDigest      string                `json:"plan_digest"`
		Paths           []string              `json:"paths"`
		Diff            mergeApprovalDiff     `json:"diff"`
	}

	// mergeApprovalDiff carries the bounded plan diff.
	mergeApprovalDiff struct {
		Status  string `json:"status"`
		Content string `json:"content,omitempty"`
		Message string `json:"message,omitempty"`
	}
)

// snapshotBarrier pauses lease-honoring writers of the main workspace by
// holding a whole-workspace write lease on behalf of the Agent.
func snapshotBarrier(leases orchestration.LeaseRepository) workspacecoord.Barrier {
	if leases == nil {
		return nil
	}
	return workspacecoord.BarrierFunc(func(ctx context.Context, teamID orchestration.TeamID, agentID orchestration.AgentID, runID orchestration.RunID) (func(), error) {
		lease, err := leases.AcquireLease(ctx, orchestration.LeaseSpec{
			TeamID: teamID, AgentID: agentID, RunID: runID, Path: orchestration.WorkspaceRootLeasePath,
			Mode: orchestration.LeaseWrite, Wait: snapshotBarrierWait,
		})
		if err != nil {
			return nil, err
		}
		return func() {
			_, _ = leases.ReleaseLease(context.WithoutCancel(ctx), lease.ID, agentID, "workspace barrier released")
		}, nil
	})
}

// mergeApprover asks the user through approver before a snapshot plan or a
// git patch changes the main workspace. Agents attributes the request to its
// child.
func mergeApprover(approver policy.Approver, agents agentLookup) workspacecoord.MergeApprover {
	if approver == nil {
		return nil
	}
	return workspacecoord.MergeApproverFunc(func(ctx context.Context, request workspacecoord.MergeApproval) (bool, error) {
		arguments, err := json.Marshal(mergeApprovalPayload(request))
		if err != nil {
			return false, fmt.Errorf("encode merge approval: %w", err)
		}
		actor := orchestration.ActorScope{TeamID: request.Merge.TeamID, AgentID: request.Merge.AgentID, TaskID: request.Merge.TaskID}
		if agents != nil {
			if agent, err := agents.GetAgent(ctx, request.Merge.AgentID); err == nil {
				actor = agent.ActorScope()
				actor.TaskID = request.Merge.TaskID
			} else if !errors.Is(err, orchestration.ErrNotFound) {
				return false, fmt.Errorf("load merge Agent: %w", err)
			}
		}
		return approver.Approve(ctx, policy.Request{
			Actor: actor, CallID: string(request.Merge.ID), ToolName: workspacecoord.MergeApplyToolName,
			Capability: "workspace.write", Resource: orchestration.WorkspaceRootLeasePath, RiskClass: "write",
			NormalizedArg: arguments,
		}, policy.Decision{
			Effect: policy.EffectRequireApproval, RuleID: mergeApplyRuleID,
			Reason: fmt.Sprintf("%s merge writes %d path(s) to the main workspace", request.Merge.Kind, len(approvalPaths(request))),
		})
	})
}

// mergeApprovalPayload describes the plan or patch of request for the
// approval card.
func mergeApprovalPayload(request workspacecoord.MergeApproval) mergeApprovalArguments {
	diff := mergeApprovalDiff{Status: "planned", Content: request.Diff}
	if request.Diff == "" {
		diff.Message = "the plan changes no file content"
	}
	candidate := request.Plan.CandidateDigest
	if candidate == "" {
		candidate = request.Merge.PatchArtifact
	}
	return mergeApprovalArguments{
		MergeID: request.Merge.ID, TaskID: request.Merge.TaskID,
		CandidateDigest: candidate, PlanDigest: request.Plan.Digest,
		Paths: approvalPaths(request), Diff: diff,
	}
}

// approvalPaths lists the paths a merge writes: the plan paths of a
// snapshot merge, or the patched paths of a git-patch merge.
func approvalPaths(request workspacecoord.MergeApproval) []string {
	if len(request.Plan.Entries) > 0 || request.Merge.Kind == workspacecoord.MergeSnapshot {
		return request.Plan.Paths()
	}
	return request.Paths
}
