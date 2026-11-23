package tool

import "context"

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

func emitLiveEvent(ctx context.Context, event LiveEvent) {
	if sink := LiveEventSinkFromContext(ctx); sink != nil {
		_ = sink(ctx, event)
	}
}
