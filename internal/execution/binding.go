package execution

import (
	"errors"
	"fmt"
	"strings"
)

var (
	ErrInvalidBinding = errors.New("invalid execution binding")
	ErrMissingScope   = errors.New("execution binding is missing its durable scope")
)

// Binding is the identity carried from the host execution into every Turn,
// Flow step, model request, tool call, policy request, and recovery record.
// String identifiers keep this package independent of a particular control
// plane's ID types.
type Binding struct {
	PlanID       string `json:"plan_id,omitempty"`
	TaskID       string `json:"task_id,omitempty"`
	ExecutionID  string `json:"execution_id,omitempty"`
	AttemptID    string `json:"attempt_id,omitempty"`
	StepID       string `json:"step_id,omitempty"`
	TurnID       string `json:"turn_id,omitempty"`
	Attempt      int    `json:"attempt,omitempty"`
	ContractHash string `json:"contract_hash,omitempty"`
}

func (binding Binding) IsZero() bool {
	return binding.PlanID == "" &&
		binding.TaskID == "" &&
		binding.ExecutionID == "" &&
		binding.AttemptID == "" &&
		binding.StepID == "" &&
		binding.TurnID == "" &&
		binding.Attempt == 0 &&
		binding.ContractHash == ""
}

func (binding Binding) IsDurable() bool {
	return binding.PlanID != "" ||
		binding.TaskID != "" ||
		binding.ExecutionID != "" ||
		binding.AttemptID != "" ||
		binding.StepID != "" ||
		binding.Attempt != 0 ||
		binding.ContractHash != ""
}

func (binding Binding) Validate() error {
	if binding.IsZero() {
		return nil
	}
	if binding.TurnID != "" && strings.TrimSpace(binding.TurnID) == "" {
		return fmt.Errorf("%w: turn ID is blank", ErrInvalidBinding)
	}
	if binding.StepID != "" && binding.ExecutionID == "" {
		return fmt.Errorf("%w: step requires execution ID", ErrMissingScope)
	}
	if binding.AttemptID != "" && binding.ExecutionID == "" {
		return fmt.Errorf("%w: attempt requires execution ID", ErrMissingScope)
	}
	if binding.IsDurable() {
		if binding.ExecutionID == "" {
			return fmt.Errorf("%w: durable binding requires execution ID", ErrMissingScope)
		}
		if binding.ContractHash == "" {
			return fmt.Errorf("%w: durable binding requires contract hash", ErrMissingScope)
		}
		if binding.Attempt < 1 {
			return fmt.Errorf("%w: durable binding requires a positive attempt", ErrInvalidBinding)
		}
	}
	if binding.Attempt < 0 {
		return fmt.Errorf("%w: attempt cannot be negative", ErrInvalidBinding)
	}
	if binding.ContractHash != "" && len(binding.ContractHash) != 64 {
		return fmt.Errorf("%w: contract hash must be a SHA-256 digest", ErrInvalidBinding)
	}
	return nil
}

// Child returns a narrower binding for a step or nested operation. It cannot
// widen the durable scope or change the contract identity.
func (binding Binding) Child(stepID, turnID string) (Binding, error) {
	child := binding
	child.StepID = stepID
	child.TurnID = turnID
	if err := child.Validate(); err != nil {
		return Binding{}, err
	}
	return child, nil
}
