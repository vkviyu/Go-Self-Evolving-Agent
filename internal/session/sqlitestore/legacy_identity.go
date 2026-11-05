package sqlitestore

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacecoord"
)

// Old migrations supplied empty request hashes. Their durable fields still
// constrain a replay: an empty hash is not permission to reuse a key with a
// different request. Do not backfill from a caller's unverified new input.
func legacySpawnMatches(agent orchestration.AgentSnapshot, spec orchestration.SpawnSpec) bool {
	// The historical display alias was not persisted, but its executable grant
	// was: require the exact profile revision/snapshot instead of trusting an
	// alias supplied by the replaying caller.
	workspace, err := agent.Workspace.Normalize()
	requested, requestErr := spec.Workspace.Normalize()
	return err == nil && requestErr == nil && workspace == requested &&
		agent.TeamID == spec.TeamID && agent.ParentAgentID == spec.ParentAgentID &&
		agent.AssignedTaskID == spec.TaskID && agent.ProfileRevision == spec.ProfileRevision &&
		agent.InitialPrompt == spec.InitialPrompt && agent.ContextPolicy == spec.ContextPolicy &&
		equalProfileJSON(agent.ProfileSnapshot, spec.ProfileSnapshot)
}

func equalProfileJSON(left, right json.RawMessage) bool {
	if len(left) == 0 || len(right) == 0 {
		return len(left) == 0 && len(right) == 0
	}
	var a, b bytes.Buffer
	return json.Compact(&a, left) == nil && json.Compact(&b, right) == nil && bytes.Equal(a.Bytes(), b.Bytes())
}

func legacyTaskMatches(ctx context.Context, query rowQuery, task orchestration.TaskSnapshot, spec orchestration.TaskSpec) (bool, error) {
	if task.TeamID != spec.TeamID || task.ParentTaskID != spec.ParentTaskID || task.Title != spec.Title || task.Description != spec.Description ||
		task.Priority != spec.Priority || task.RequiredProfile != spec.RequiredProfile || task.VerificationProfile != spec.VerificationProfile {
		return false, nil
	}
	var actor string
	err := query.QueryRowContext(ctx, `SELECT actor_agent_id FROM orchestration_events WHERE stream_type = ? AND stream_id = ? AND event_type = ? ORDER BY stream_sequence LIMIT 1`,
		string(orchestration.StreamTask), string(task.ID), string(orchestration.EventTaskCreated)).Scan(&actor)
	if err != nil {
		return false, recordError(err, "task creation evidence", string(task.ID))
	}
	return actor == string(spec.ActorAgentID), nil
}

func legacyWaitMatches(wait orchestration.WaitSet, spec orchestration.WaitSpec) bool {
	sameDeadline := wait.Deadline == nil && spec.Deadline == nil || wait.Deadline != nil && spec.Deadline != nil && wait.Deadline.Equal(*spec.Deadline)
	return wait.TeamID == spec.TeamID && wait.AgentID == spec.AgentID && wait.RunID == spec.RunID && wait.ToolCallID == spec.ToolCallID &&
		wait.Condition == spec.Condition && slices.Equal(wait.TargetAgentIDs, spec.TargetAgentIDs) && wait.Any == spec.Any && sameDeadline && wait.ResumeToken == spec.ResumeToken
}

func legacyViewMatches(view workspacecoord.View, spec workspacecoord.ViewSpec) bool {
	return view.TeamID == spec.TeamID && view.AgentID == spec.AgentID && view.TaskID == spec.TaskID && view.Mode == spec.Mode &&
		view.WorkspaceRoot == spec.WorkspaceRoot && view.RootPath == spec.RootPath && view.BaseRevision == spec.BaseRevision
}

func legacyMergeMatches(merge workspacecoord.MergeRequest, spec workspacecoord.MergeSpec) bool {
	kind := spec.Kind
	if kind == "" {
		kind = workspacecoord.MergeGitPatch
	}
	return merge.TeamID == spec.TeamID && merge.AgentID == spec.AgentID && merge.TaskID == spec.TaskID && merge.ViewID == spec.ViewID &&
		merge.BaseRevision == spec.BaseRevision && merge.PatchArtifact == spec.PatchArtifact && merge.Kind == kind &&
		merge.CandidateDigest == spec.CandidateDigest && merge.Supersedes == spec.Supersedes
}
