package orchestration

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
)

// ExecutionStop is a durable coordination signal. It preserves the original
// Agent run so a coordinator can pause and resume it without creating a new
// task attempt.
type ExecutionStop struct {
	Kind  ExecutionStopKind
	Cause error
}
type ExecutionStopKind string

const (
	ExecutionTransport           ExecutionStopKind = "transport"
	ExecutionRateLimit           ExecutionStopKind = "rate_limit"
	ExecutionProviderUnavailable ExecutionStopKind = "provider_unavailable"
	ExecutionCredentials         ExecutionStopKind = "credentials"
	ExecutionQuota               ExecutionStopKind = "quota"
	ExecutionBudget              ExecutionStopKind = "budget"
	ExecutionOutcomeUnknown      ExecutionStopKind = "outcome_unknown"
	ExecutionToolCallFormat      ExecutionStopKind = "model_tool_format"
)

func (stop *ExecutionStop) Error() string {
	if stop == nil {
		return "execution paused"
	}
	if stop.Cause == nil {
		return fmt.Sprintf("execution paused (%s)", stop.Kind)
	}
	return fmt.Sprintf("execution paused (%s): %v", stop.Kind, stop.Cause)
}
func (stop *ExecutionStop) Unwrap() error {
	if stop == nil {
		return nil
	}
	return stop.Cause
}

// ExecutionStopKindOf unwraps cause and reports whether it represents a recognized
// interruption of execution.
func ExecutionStopKindOf(cause error) (ExecutionStopKind, bool) {
	var stop *ExecutionStop
	if !errors.As(cause, &stop) || stop == nil {
		return "", false
	}
	switch stop.Kind {
	case ExecutionTransport, ExecutionRateLimit, ExecutionProviderUnavailable, ExecutionCredentials, ExecutionQuota, ExecutionBudget, ExecutionOutcomeUnknown, ExecutionToolCallFormat:
		return stop.Kind, true
	default:
		return "", false
	}
}

func FromModelExecutionError(cause error) error {
	if cause == nil || errors.Is(cause, context.Canceled) {
		return cause
	}
	if _, ok := ExecutionStopKindOf(cause); ok {
		return cause
	}
	var status interface{ StatusCode() int }
	if errors.As(cause, &status) {
		switch code := status.StatusCode(); {
		case code == 401 || code == 403:
			return &ExecutionStop{Kind: ExecutionCredentials, Cause: cause}
		case code == 402:
			return &ExecutionStop{Kind: ExecutionQuota, Cause: cause}
		case code == 408 || code == 425 || code == 429:
			return &ExecutionStop{Kind: ExecutionRateLimit, Cause: cause}
		case code >= 500 && code <= 599:
			return &ExecutionStop{Kind: ExecutionProviderUnavailable, Cause: cause}
		}
	}
	if errors.Is(cause, context.DeadlineExceeded) || errors.Is(cause, io.EOF) || errors.Is(cause, io.ErrUnexpectedEOF) {
		return &ExecutionStop{Kind: ExecutionTransport, Cause: cause}
	}
	var network net.Error
	if errors.As(cause, &network) && (network.Timeout() || network.Temporary()) {
		return &ExecutionStop{Kind: ExecutionTransport, Cause: cause}
	}
	for _, failure := range []syscall.Errno{syscall.ECONNRESET, syscall.ECONNREFUSED, syscall.ECONNABORTED, syscall.ENETDOWN, syscall.ENETUNREACH, syscall.EHOSTUNREACH, syscall.EPIPE} {
		if errors.Is(cause, failure) {
			return &ExecutionStop{Kind: ExecutionTransport, Cause: cause}
		}
	}
	return cause
}
