package orchestration

import (
	"errors"
	"fmt"
)

var (
	ErrTaskReassignUnsafe = errors.New("task attempt cannot be safely reassigned")
	ErrTaskAttemptLimit   = errors.New("task attempt limit reached")
)

// MaxTaskAttempts bounds new executions of one Task, including its first run.
// Transport/provider interruptions resume that run and do not consume attempts.
const MaxTaskAttempts = 3

type TaskAttemptFailureCode string

const TaskAttemptIterationLimit TaskAttemptFailureCode = "iteration_limit"

// Retryable reports whether this failure code permits another attempt under the task retry
// policy.
func (code TaskAttemptFailureCode) Retryable() bool {
	return code == TaskAttemptIterationLimit
}

// TaskAttemptFailure is produced by trusted runtime code for a known terminal
// attempt failure. Model output and arbitrary provider errors cannot opt in.
// This classification only permits a safety check; it does not prove that
// replaying the task is safe or override the repository's effect checks.
type TaskAttemptFailure struct {
	Code  TaskAttemptFailureCode
	Cause error
}

func (failure *TaskAttemptFailure) Error() string {
	return fmt.Sprintf("task attempt %s: %v", failure.Code, failure.Cause)
}

func (failure *TaskAttemptFailure) Unwrap() error { return failure.Cause }
