package tool

import "fmt"

// OutcomeUnknownError is fatal to automatic execution. A started invocation
// may have changed external state even when no successful result was recorded.
// A caller must reconcile that state before authorizing a new operation.
type OutcomeUnknownError struct {
	CallID string
	Tool   string
	Cause  error
}

func (*OutcomeUnknownError) ToolExecutionFatal() bool { return true }

func (err *OutcomeUnknownError) Error() string {
	message := fmt.Sprintf("tool outcome unknown for %s (call %s); inspect actual state before retrying; automatic replay is refused", err.Tool, err.CallID)
	if err.Cause != nil {
		message += ": " + err.Cause.Error()
	}
	return message
}

func (err *OutcomeUnknownError) Unwrap() error { return err.Cause }

func mayHaveSideEffects(risk RiskClass) bool {
	return risk != RiskPure && risk != RiskRead
}
