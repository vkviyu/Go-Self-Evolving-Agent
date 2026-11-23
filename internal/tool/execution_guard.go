package tool

import (
	"context"
	"errors"
	"fmt"
)

var (
	ErrExecutionBindingRequired  = errors.New("tool call requires a durable execution binding")
	ErrExecutionIdentityConflict = errors.New("tool call execution identity conflicts with its actor")
)

// ExecutionBoundary enforces the boundary between an ordinary conversation
// and host-owned durable work. Read-only and pure tools may run in a direct
// conversation. A child assigned to a Task must carry the immutable execution
// binding before it can invoke a side-effecting tool.
type ExecutionBoundary struct{}

func (ExecutionBoundary) CheckCall(_ context.Context, call Call, descriptor Descriptor) error {
	if err := call.Execution.Validate(); err != nil {
		return fmt.Errorf("validate execution binding: %w", err)
	}
	if call.Execution.IsZero() {
		if sideEffecting(descriptor.RiskClass) && call.Actor.TaskID != "" {
			return fmt.Errorf("%w: %s", ErrExecutionBindingRequired, descriptor.Name)
		}
		return nil
	}
	if call.Execution.TaskID != "" && call.Actor.TaskID != "" &&
		call.Execution.TaskID != call.Actor.TaskID {
		return fmt.Errorf("%w: task ID differs from actor scope", ErrExecutionIdentityConflict)
	}
	if call.Execution.ExecutionID != "" && call.Actor.RunID != "" &&
		call.Execution.ExecutionID != string(call.Actor.RunID) {
		return fmt.Errorf("%w: execution ID differs from Agent run ID", ErrExecutionIdentityConflict)
	}
	return nil
}

func (ExecutionBoundary) BeforeStart(context.Context, Call, Descriptor) (func(), error) {
	return nil, nil
}

func sideEffecting(risk RiskClass) bool {
	switch risk {
	case RiskWrite, RiskExecute, RiskSecret:
		return true
	default:
		return false
	}
}

var _ ExecutionGuard = ExecutionBoundary{}
