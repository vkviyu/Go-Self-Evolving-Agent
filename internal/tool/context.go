package tool

import (
	"context"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

// Invocation identifies the trusted execution envelope for a tool call. Tool
// arguments remain model-controlled; these values are attached by Pipeline and
// therefore cannot be forged by a caller.
type Invocation struct {
	SessionID          session.ID
	TurnID             string
	Execution          orchestration.ExecutionBinding
	RuntimeSnapshotID  string
	CallID             string
	RegistryGeneration uint64
}

type invocationContextKey struct{}

func withInvocation(ctx context.Context, invocation Invocation) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, invocationContextKey{}, invocation)
}

// InvocationFromContext returns the trusted tool execution envelope.
func InvocationFromContext(ctx context.Context) (Invocation, bool) {
	if ctx == nil {
		return Invocation{}, false
	}
	invocation, ok := ctx.Value(invocationContextKey{}).(Invocation)
	return invocation, ok
}

type pipelineContextKey struct{}

func withPipeline(ctx context.Context, pipeline *Pipeline) context.Context {
	return context.WithValue(ctx, pipelineContextKey{}, pipeline)
}

// PipelineFromContext returns the exact caller-specific execution view. Flow
// tools use it to inherit interactive or fail-closed approval behavior instead
// of falling back to a process-global approver.
func PipelineFromContext(ctx context.Context) (*Pipeline, bool) {
	if ctx == nil {
		return nil, false
	}
	pipeline, ok := ctx.Value(pipelineContextKey{}).(*Pipeline)
	return pipeline, ok && pipeline != nil
}
