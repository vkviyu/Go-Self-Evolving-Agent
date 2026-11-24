package agentruntime

import (
	"context"
	"errors"

	"github.com/cloudwego/eino/schema"

	"github.com/vkviyu/go-self-evolving-agent/internal/observation"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

func (f *Factory) observedContext(ctx context.Context, id session.ID, turn string) context.Context {
	scope := observation.Scope{SessionID: string(id), TurnID: turn}
	if f != nil {
		actor := f.config.ActorScope
		scope.TeamID, scope.AgentID, scope.ParentAgentID = string(actor.TeamID), string(actor.AgentID), string(actor.ParentAgentID)
		scope.TaskID, scope.RunID = string(actor.TaskID), string(actor.RunID)
	}
	return observation.WithExactScope(ctx, scope)
}

func (t *Turn) observedContext(ctx context.Context) context.Context {
	var f *Factory
	if t.recordedModel != nil {
		f = t.recordedModel.factory
	}
	return f.observedContext(ctx, t.sessionID, t.turnID)
}

// Compaction and maintenance can run under their own durable turn identity.
// Retain the link to the triggering execution before entering that request.
func (m *recordingModel) observeRequestContext(ctx context.Context, requestID string) context.Context {
	if !observation.Enabled(ctx) {
		return ctx
	}
	scope := observation.ScopeFrom(ctx)
	if scope.TurnID != "" && (scope.TurnID != m.turnID || scope.SessionID != string(m.sessionID)) {
		observation.Emit(ctx, observation.Event{
			Component: "runtime", Name: "model.request.linked", Scope: observation.Scope{RequestID: requestID},
			Labels: map[string]string{"request_turn_id": m.turnID, "request_session_id": string(m.sessionID), "purpose": m.purpose},
		})
	}
	return observation.WithScope(ctx, observation.Scope{SessionID: string(m.sessionID), TurnID: m.turnID, RequestID: requestID})
}

// emitStage observes host lifecycle boundaries. Text deltas go directly to
// their presentation sink and never enter the diagnostic recorder.
func emitStage(ctx context.Context, sink EventSink, stage Event) error {
	if observation.Enabled(ctx) {
		ev := observation.Event{Component: "runtime", Name: string(stage.Type), Scope: observation.Scope{TurnID: stage.TurnID}}
		if stage.Purpose != "" {
			ev.Labels = map[string]string{"purpose": stage.Purpose}
		}
		observation.Emit(ctx, ev)
	}
	return emitEvent(ctx, sink, stage)
}

func observeTurnFinished(ctx context.Context, err error) {
	if !observation.Enabled(ctx) {
		return
	}
	ev := observation.Event{Component: "runtime", Name: "turn.finished", Status: "completed"}
	var suspended *orchestration.WaitSuspendedError
	switch {
	case err == nil:
	case errors.As(err, &suspended):
		ev.Status = "suspended"
	case errors.Is(err, context.Canceled):
		ev.Status = "canceled"
	default:
		if kind, ok := orchestration.ExecutionStopKindOf(err); ok {
			ev.Status = "paused"
			ev.Labels = map[string]string{"reason": string(kind)}
		} else {
			ev.Status = "failed"
		}
	}
	observation.Emit(ctx, ev)
}

func observeModelFinished(ctx context.Context, message *schema.Message, err error) {
	if !observation.Enabled(ctx) {
		return
	}
	ev := observation.Event{Component: "runtime", Name: "model.request.finished", Status: "completed"}
	if err != nil {
		ev.Status = "failed"
	} else {
		ev.Metrics = map[string]int64{"text_bytes": int64(len(message.Content)), "reasoning_bytes": int64(len(message.ReasoningContent)), "tool_calls": int64(len(message.ToolCalls))}
		for _, call := range message.ToolCalls {
			if call.ID != "" {
				observation.Emit(ctx, observation.Event{Component: "runtime", Name: "model.tool_call", Scope: observation.Scope{CallID: call.ID}})
			}
		}
	}
	observation.Emit(ctx, ev)
}
