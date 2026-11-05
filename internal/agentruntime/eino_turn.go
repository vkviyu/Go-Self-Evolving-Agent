package agentruntime

import (
	"context"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/compose"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentgoal"
	"github.com/vkviyu/go-self-evolving-agent/internal/einoadapter"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

// buildTurn is the single Eino assembly point for new and resumed Turns.
// It does not select strategies or discover recovery metadata.
func (f *Factory) buildTurn(ctx context.Context, sessionID session.ID, turnID, runtimeSnapshotID string, snapshot *tool.Snapshot, requestSequence uint64) (*Turn, error) {
	return f.buildTurnWithAdmission(ctx, sessionID, turnID, runtimeSnapshotID, snapshot, requestSequence, "")
}

func (f *Factory) buildTurnWithAdmission(ctx context.Context, sessionID session.ID, turnID, runtimeSnapshotID string, snapshot *tool.Snapshot, requestSequence uint64, admissionToken string) (*Turn, error) {
	einoTools, err := einoadapter.NewToolsWithActor(f.pipeline, snapshot, sessionID, runtimeSnapshotID, turnID, f.config.ActorScope)
	if err != nil {
		return nil, err
	}
	{
		filtered := einoTools[:0]
		for _, candidate := range einoTools {
			info, err := candidate.Info(ctx)
			if err != nil {
				return nil, fmt.Errorf("inspect turn tool: %w", err)
			}
			hideReport := info.Name == agentgoal.ReportToolName && !f.goalMode.Enabled
			hideStart := info.Name == agentgoal.StartToolName && (!f.goalMode.Enabled || f.goalMode.Activation == agentgoal.ActivationAlways)
			if !hideReport && !hideStart {
				if f.profile != nil {
					binding, ok := snapshot.Get(info.Name)
					if !ok || !permitsProfileTool(*f.profile, binding.Descriptor) {
						continue
					}
				}
				filtered = append(filtered, candidate)
			}
		}
		einoTools = filtered
	}
	recordedModel := &recordingModel{
		inner:             f.model,
		factory:           f,
		events:            f.events,
		sessionID:         sessionID,
		turnID:            turnID,
		runtimeSnapshotID: runtimeSnapshotID,
	}
	recordedModel.requestSequence.Store(requestSequence)
	agent, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
		Name:          f.config.Name,
		Description:   f.config.Description,
		Instruction:   f.runtimeInstruction(),
		Model:         recordedModel,
		MaxIterations: f.config.MaxIterations,
		ToolsConfig: adk.ToolsConfig{ToolsNodeConfig: compose.ToolsNodeConfig{
			Tools:               einoTools,
			ExecuteSequentially: true,
		}},
	})
	if err != nil {
		return nil, fmt.Errorf("construct Eino ChatModelAgent: %w", err)
	}
	return &Turn{
		recordedModel:     recordedModel,
		strategyID:        f.strategyID,
		baselineConfigID:  f.baselineConfigID,
		strategyFailure:   f.strategyFailure(),
		runner:            adk.NewRunner(ctx, adk.RunnerConfig{Agent: agent, EnableStreaming: true, CheckPointStore: f.checkpoints}),
		events:            f.events,
		sessionID:         sessionID,
		turnID:            turnID,
		runtimeSnapshotID: runtimeSnapshotID,
		modelProfile:      f.config.ModelProfile,
		provider:          f.config.Provider,
		model:             f.config.Model,
		goalMode:          f.goalMode.Enabled,
		goalActivation:    f.goalMode.Activation,
		admissionToken:    admissionToken,
		checkpointID:      checkpointID(sessionID, turnID),
		checkpoints:       f.checkpoints,
		executionConfigID: f.executionConfigID(),
		runID:             f.config.ActorScope.RunID,
		mailbox:           f.mailbox,
	}, nil
}

func (f *Factory) runtimeInstruction() string {
	instruction := f.config.Instruction
	if f.goalMode.Enabled {
		protocol := agentgoal.ProtocolInstruction
		if f.goalMode.Activation == agentgoal.ActivationAuto {
			protocol = agentgoal.AdmissionInstruction
		}
		instruction = strings.TrimSpace(instruction) + "\n\n" + protocol
	}
	return instruction
}
