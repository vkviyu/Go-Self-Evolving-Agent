package agentruntime

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentgoal"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

// Turn is a single-use Eino runner bound to one registry generation.
type Turn struct {
	runner            *adk.Runner
	events            session.EventStore
	sessionID         session.ID
	turnID            string
	runtimeSnapshotID string
	modelProfile      string
	provider          string
	model             string
	executionConfigID string
	strategyID        string
	baselineConfigID  string
	strategyFailure   func(context.Context, error) error
	runID             orchestration.RunID
	goalMode          bool
	goalActivation    agentgoal.Activation
	admissionToken    string
	admissionBind     func(context.Context, string, string) error
	checkpointID      string
	checkpoints       session.CheckpointStore
	started           atomic.Bool
	recordedModel     *recordingModel
	mailbox           BackgroundMailbox
	journal           *journalReplay
}

// Result contains the final assistant response emitted by the loop.
type Result struct {
	TurnID            string
	RuntimeSnapshotID string
	Final             *schema.Message
	Streamed          bool
	GoalID            string
	GoalState         agentgoal.State
	GoalDecision      agentgoal.Decision
	GoalStopReason    agentgoal.StopReason
	GoalRounds        int
	GoalReason        string
	ContextUsage      *ContextUsage
}

// Run executes an Eino loop using the supplied conversation messages. The
// final message must be a new user message; earlier messages are model context.
func (t *Turn) Run(ctx context.Context, messages []*schema.Message, sink EventSink) (Result, error) {
	return t.RunWithAdmission(ctx, messages, sink, nil)
}

// RunWithAdmission records the turn and its user input before invoking an
// admission callback. This makes Goal round registration a recoverable part of
// the same durable turn rather than a gap before persistence.
func (t *Turn) RunWithAdmission(ctx context.Context, messages []*schema.Message, sink EventSink, before func(string) error) (Result, error) {
	if err := t.begin(ctx); err != nil {
		return Result{}, err
	}
	if len(messages) == 0 || messages[len(messages)-1] == nil || messages[len(messages)-1].Role != schema.User {
		return Result{}, errors.New("Agent turn requires a final user message")
	}
	user := messages[len(messages)-1]
	return t.start(ctx, messages, sink, "", session.PendingEvent{
		Type:              session.EventUserMessage,
		CorrelationID:     t.turnID,
		RuntimeSnapshotID: t.runtimeSnapshotID,
		Payload: session.MustJSONPayload(map[string]any{
			"turn_id": t.turnID,
			"content": user.Content,
		}),
	}, before)
}

// SetAdmission binds this Turn to a host-issued durable Goal admission. The
// callback is deliberately owned by Conversation/its durable store so the
// runtime does not depend on a particular Goal database implementation.
func (t *Turn) SetAdmission(token string, bind func(context.Context, string, string) error) {
	t.admissionToken, t.admissionBind = token, bind
}

// RunBackground executes a wake turn whose input is a block of Agent messages
// already claimed for this turn. history holds the prepared model context.
func (t *Turn) RunBackground(ctx context.Context, history []*schema.Message, input BackgroundInput, sink EventSink) (Result, error) {
	if err := t.begin(ctx); err != nil {
		return Result{}, err
	}
	if len(input.MessageIDs) == 0 || input.Content == "" {
		return Result{}, ErrNoBackgroundInput
	}
	messages := append(append([]*schema.Message(nil), history...), schema.UserMessage(input.Content))
	return t.start(ctx, messages, sink, TurnInputBackground, session.PendingEvent{
		Type:              session.EventBackgroundInput,
		CorrelationID:     t.turnID,
		RuntimeSnapshotID: t.runtimeSnapshotID,
		Payload: session.MustJSONPayload(backgroundRecord{
			TurnID: t.turnID, Content: input.Content, MessageIDs: input.MessageIDs, Opening: true,
		}),
	}, nil)
}

// begin refuses a tombstoned turn and claims this single-use Turn.
func (t *Turn) begin(ctx context.Context) error {
	if err := session.CheckRecovery(ctx, t.events, t.sessionID, t.turnID); err != nil {
		return err
	}
	if !t.started.CompareAndSwap(false, true) {
		return errors.New("Agent turn is single-use")
	}
	return nil
}

// cancelOnDone returns a run option that cancels the Eino run immediately
// once ctx is done, and the function that stops watching ctx.
func cancelOnDone(ctx context.Context) (adk.AgentRunOption, func() bool) {
	cancelOption, cancel := adk.WithCancel()
	stop := context.AfterFunc(ctx, func() {
		handle, _ := cancel(adk.WithAgentCancelMode(adk.CancelImmediate))
		if handle != nil {
			_ = handle.Wait()
		}
	})
	return cancelOption, stop
}

// start records the turn start with its opening input and runs Eino.
func (t *Turn) start(ctx context.Context, messages []*schema.Message, sink EventSink, inputKind string, opening session.PendingEvent, before func(string) error) (Result, error) {
	userContent := ""
	if len(messages) > 0 && messages[len(messages)-1] != nil {
		userContent = messages[len(messages)-1].Content
	}
	started := map[string]any{
		"turn_id": t.turnID, "model_profile": t.modelProfile,
		"provider": t.provider, "model": t.model,
		"execution_config_id":      t.executionConfigID,
		"loop_spec_id":             t.strategyID,
		"base_execution_config_id": t.baselineConfigID,
		"run_id":                   t.runID,
		"goal_mode":                t.goalMode,
		"goal_activation":          t.goalActivation,
		"admission_token":          t.admissionToken,
		"journal":                  turnJournal{Version: 1, Input: messages, Prepared: true},
	}
	if inputKind != "" {
		started["input"] = inputKind
	}
	if _, err := t.events.Append(ctx, t.sessionID, session.AnySequence,
		session.PendingEvent{
			Type:              session.EventTurnStarted,
			CorrelationID:     t.turnID,
			RuntimeSnapshotID: t.runtimeSnapshotID,
			Payload:           session.MustJSONPayload(started),
		},
		opening,
	); err != nil {
		return Result{}, fmt.Errorf("record Agent turn start: %w", err)
	}
	if t.admissionToken != "" {
		if t.admissionBind == nil {
			return Result{}, t.fail(ctx, errors.New("durable Goal admission has no binding handler"))
		}
		if err := t.admissionBind(ctx, t.turnID, userContent); err != nil {
			return Result{}, t.fail(ctx, fmt.Errorf("bind Goal admission to turn: %w", err))
		}
	}
	if before != nil {
		if err := before(t.turnID); err != nil {
			return Result{}, t.fail(ctx, fmt.Errorf("turn admission: %w", err))
		}
	}

	cancelOption, stopCancelWatch := cancelOnDone(ctx)
	defer stopCancelWatch()
	iterator := t.runner.Run(ctx, messages, cancelOption, adk.WithCheckPointID(t.checkpointID))
	return t.consumeIterator(ctx, iterator, sink, false)
}

// ID returns the durable turn identity used by the checkpoint store.
func (t *Turn) ID() string {
	if t == nil {
		return ""
	}
	return t.turnID
}

// Resume continues this turn from its persisted Eino checkpoint.
func (t *Turn) Resume(ctx context.Context, sink EventSink) (Result, error) {
	if err := t.begin(ctx); err != nil {
		return Result{}, err
	}
	if t.checkpointID == "" {
		return Result{}, errors.New("Agent turn has no checkpoint ID")
	}
	cancelOption, stopCancelWatch := cancelOnDone(ctx)
	defer stopCancelWatch()
	if t.journal != nil {
		iterator := t.runner.Run(ctx, t.journal.Input, cancelOption, adk.WithCheckPointID(t.checkpointID))
		return t.consumeIterator(ctx, iterator, sink, true)
	}
	iterator, err := t.runner.Resume(ctx, t.checkpointID, cancelOption)
	if err != nil {
		return Result{}, t.fail(ctx, err)
	}
	return t.consumeIterator(ctx, iterator, sink, true)
}
