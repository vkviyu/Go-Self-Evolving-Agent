package orchestration

import (
	"errors"
	"testing"
)

type timeoutError struct{}

func (timeoutError) Error() string   { return "timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestExecutionStopClassifiesOnlyRecoverableProviderFailures(t *testing.T) {
	if stop, ok := ExecutionStopKindOf(FromModelExecutionError(timeoutError{})); !ok || stop != ExecutionTransport {
		t.Fatalf("timeout classification = %q, %v", stop, ok)
	}
	if stop, ok := ExecutionStopKindOf(FromModelExecutionError(errors.New("invalid request"))); ok || stop != "" {
		t.Fatalf("configuration error was classified as %q", stop)
	}
	if stop, ok := ExecutionStopKindOf(FromModelExecutionError(errors.New("context canceled"))); ok || stop != "" {
		t.Fatalf("ordinary error was classified as %q", stop)
	}
}
