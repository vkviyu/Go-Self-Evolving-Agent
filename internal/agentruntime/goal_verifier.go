package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentgoal"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

// fileGoalVerifier deliberately uses the ordinary policy-controlled read tool,
// actor scope and frozen registry. It has no privileged filesystem back door.
type fileGoalVerifier struct{ factory *Factory }

func (verifier *fileGoalVerifier) Verify(ctx context.Context, evaluation agentgoal.Evaluation) (agentgoal.Verification, error) {
	checked := agentgoal.Verification{Complete: true, Satisfied: true}
	if evaluation.Goal.Contract == nil {
		checked.Complete = false
		return checked, nil
	}
	if err := evaluation.Goal.Contract.Validate(); err != nil {
		return checked, err
	}
	f := verifier.factory
	if err := f.CheckTurnRecovery(ctx, evaluation.Goal.SessionID, evaluation.Round.TurnID); err != nil {
		return checked, err
	}
	var snapshot *tool.Snapshot
	var snapshotID string
	for index, criterion := range evaluation.Goal.Contract.Criteria {
		if criterion.Kind != "file_content" {
			checked.Complete = false
			continue
		}
		if snapshot == nil {
			var err error
			snapshotID, err = f.snapshotIDForTurn(ctx, evaluation.Goal.SessionID, evaluation.Round.TurnID)
			if err != nil {
				return checked, err
			}
			snapshot, err = f.registry.ResolveSnapshot(ctx, snapshotID)
			if err != nil {
				return checked, err
			}
			binding, ok := snapshot.Get("workspace_read")
			// v3 adds truthful bounded-fingerprint metadata, while preserving
			// the content/truncated contract used here. Keep v2 for frozen
			// snapshots; unknown revisions still require explicit review.
			revision := binding.Descriptor.Revision
			if !ok || binding.Descriptor.RiskClass != tool.RiskRead || (revision != "builtin-v2" && revision != "builtin-v3") {
				return checked, errors.New("file verification requires the supported read-only workspace_read tool")
			}
		}
		args, _ := json.Marshal(map[string]any{"path": criterion.Path, "max_bytes": 64*1024 + 1})
		// Fresh reads must not replay a pre-crash observation as current state.
		callID, err := randomID()
		if err != nil {
			return checked, err
		}
		result, err := f.pipeline.Execute(ctx, tool.Call{
			Actor: f.config.ActorScope, SessionID: evaluation.Goal.SessionID, TurnID: evaluation.Round.TurnID,
			RuntimeSnapshotID: snapshotID, Registry: snapshot, CallID: "verify-" + callID,
			ToolName: "workspace_read", Arguments: args,
		})
		if errors.Is(err, os.ErrNotExist) {
			checked.Satisfied = false
			checked.Evidence = append(checked.Evidence, fmt.Sprintf("criterion %d failed: %s does not exist", index+1, criterion.Path))
			continue
		}
		if err != nil {
			return checked, fmt.Errorf("verify criterion %d: %w", index+1, err)
		}
		var read struct {
			Content   *string `json:"content"`
			Truncated *bool   `json:"truncated"`
		}
		if err := json.Unmarshal(result.Data, &read); err != nil {
			return checked, err
		}
		if read.Content == nil || read.Truncated == nil {
			return checked, errors.New("verification received an invalid workspace_read result")
		}
		passed := !*read.Truncated && *read.Content == *criterion.Content
		checked.Satisfied = checked.Satisfied && passed
		checked.Evidence = append(checked.Evidence, fmt.Sprintf("criterion %d exact file content %s: passed=%t (trusted tool call %s)", index+1, criterion.Path, passed, result.CallID))
	}
	return checked, nil
}
