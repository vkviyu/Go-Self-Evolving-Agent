package tool

import (
	"context"

	"github.com/vkviyu/go-self-evolving-agent/internal/observation"
	"github.com/vkviyu/go-self-evolving-agent/internal/policy"
)

// LiveEventType identifies a presentation-only tool lifecycle transition.
// Durable session.Event values remain the source of truth for recovery.
type LiveEventType string

const (
	LiveToolProposed       LiveEventType = "tool.proposed"
	LivePolicyChecking     LiveEventType = "tool.policy.checking"
	LivePolicyDecided      LiveEventType = "tool.policy.decided"
	LiveApprovalRequested  LiveEventType = "tool.approval.requested"
	LiveApprovalWaiting    LiveEventType = "tool.approval.waiting"
	LiveApprovalResolved   LiveEventType = "tool.approval.resolved"
	LiveToolStarted        LiveEventType = "tool.started"
	LiveToolSucceeded      LiveEventType = "tool.succeeded"
	LiveToolFailed         LiveEventType = "tool.failed"
	LiveToolOutcomeUnknown LiveEventType = "tool.outcome.unknown"
)

type LiveEvent struct {
	Type     LiveEventType
	CallID   string
	TurnID   string
	ToolName string
	Approved bool
	Effect   policy.Effect
	Err      error
}

type LiveEventSink func(context.Context, LiveEvent) error

type liveSinkKey struct{}

func WithLiveEventSink(ctx context.Context, sink LiveEventSink) context.Context {
	if sink == nil {
		return ctx
	}
	return context.WithValue(ctx, liveSinkKey{}, sink)
}

func LiveEventSinkFromContext(ctx context.Context) LiveEventSink {
	if ctx == nil {
		return nil
	}
	sink, _ := ctx.Value(liveSinkKey{}).(LiveEventSink)
	return sink
}

// emitLiveEvent forwards tool progress to the observer and UI sink without propagating
// sink errors into execution.
func emitLiveEvent(ctx context.Context, event LiveEvent) {
	if observation.Enabled(ctx) {
		var status string
		if event.Type == LivePolicyDecided {
			switch event.Effect {
			case policy.EffectAllow, policy.EffectDeny, policy.EffectRequireApproval:
				status = string(event.Effect)
			}
		}
		if event.Type == LiveApprovalResolved {
			status = "denied"
			if event.Approved {
				status = "approved"
			}
		}
		observation.Emit(ctx, observation.Event{Component: "tool", Name: string(event.Type), Status: status, Labels: map[string]string{"tool": event.ToolName}})
	}
	if sink := LiveEventSinkFromContext(ctx); sink != nil {
		_ = sink(ctx, event)
	}
}
