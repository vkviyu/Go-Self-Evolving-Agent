package agentmanager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacecoord"
)

type mergeListToolArgs struct {
	States []string `json:"states"`
}

type mergeRequeueToolArgs struct {
	MergeID          string `json:"merge_id"`
	ResolutionTaskID string `json:"resolution_task_id"`
}

type mergeRunToolArgs struct {
	Limit int `json:"limit"`
}

func (supervisor *Supervisor) mergeListTool(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	actor, err := actorFromContext(ctx)
	if err != nil {
		return nil, err
	}
	var args mergeListToolArgs
	if err := decodeToolArgs(raw, &args); err != nil {
		return nil, err
	}
	query := workspacecoord.MergeQuery{TeamID: actor.TeamID}
	for _, state := range args.States {
		query.States = append(query.States, workspacecoord.MergeState(state))
	}
	merges, err := supervisor.ListMerges(ctx, query)
	if err != nil {
		return nil, err
	}
	return json.Marshal(merges)
}

func (supervisor *Supervisor) mergeRequeueTool(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	actor, err := actorFromContext(ctx)
	if err != nil {
		return nil, err
	}
	var args mergeRequeueToolArgs
	if err := decodeToolArgs(raw, &args); err != nil {
		return nil, err
	}
	if strings.TrimSpace(args.MergeID) == "" || strings.TrimSpace(args.ResolutionTaskID) == "" {
		return nil, errors.New("merge_id and resolution_task_id are required")
	}
	merges, err := supervisor.ListMerges(ctx, workspacecoord.MergeQuery{TeamID: actor.TeamID})
	if err != nil {
		return nil, err
	}
	owned := false
	for _, candidate := range merges {
		if candidate.ID == orchestration.MergeID(args.MergeID) {
			owned = candidate.AgentID == actor.AgentID || actor.ParentAgentID == ""
			break
		}
	}
	if !owned {
		return nil, fmt.Errorf("merge %s is outside the current Agent scope", args.MergeID)
	}
	updated, err := supervisor.RequeueResolvedMerge(ctx, orchestration.MergeID(args.MergeID), orchestration.TaskID(args.ResolutionTaskID))
	if err != nil {
		return nil, err
	}
	return json.Marshal(updated)
}

func (supervisor *Supervisor) mergeRunTool(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	actor, err := actorFromContext(ctx)
	if err != nil {
		return nil, err
	}
	if actor.ParentAgentID != "" {
		return nil, errors.New("merge_run is only available to the root Agent")
	}
	var args mergeRunToolArgs
	if err := decodeToolArgs(raw, &args); err != nil {
		return nil, err
	}
	merges, err := supervisor.RunPendingMerges(ctx, actor.TeamID, args.Limit)
	if err != nil {
		return nil, err
	}
	return json.Marshal(merges)
}
