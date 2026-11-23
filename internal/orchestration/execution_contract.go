package orchestration

import (
	"github.com/vkviyu/go-self-evolving-agent/internal/execution"
)

type ExecutionContract = execution.Contract
type VerificationPolicy = execution.VerificationPolicy

const (
	VerificationNone        = execution.VerificationNone
	VerificationIndependent = execution.VerificationIndependent
	VerificationProfile     = execution.VerificationProfile
)

// ExecutionBinding identifies the durable execution scope attached to a Turn
// or tool call. Empty identifiers mean the call is a direct conversation.
type ExecutionBinding struct {
	PlanID       string `json:"plan_id,omitempty"`
	TaskID       TaskID `json:"task_id,omitempty"`
	ExecutionID  string `json:"execution_id,omitempty"`
	AttemptID    string `json:"attempt_id,omitempty"`
	StepID       string `json:"step_id,omitempty"`
	TurnID       string `json:"turn_id,omitempty"`
	Attempt      int    `json:"attempt,omitempty"`
	ContractHash string `json:"contract_hash,omitempty"`
}

func (binding ExecutionBinding) IsZero() bool {
	return binding.PlanID == "" && binding.TaskID == "" && binding.ExecutionID == "" &&
		binding.AttemptID == "" && binding.StepID == "" && binding.TurnID == "" &&
		binding.Attempt == 0 && binding.ContractHash == ""
}

func (binding ExecutionBinding) Validate() error {
	return (execution.Binding{
		PlanID:       binding.PlanID,
		TaskID:       string(binding.TaskID),
		ExecutionID:  binding.ExecutionID,
		AttemptID:    binding.AttemptID,
		StepID:       binding.StepID,
		TurnID:       binding.TurnID,
		Attempt:      binding.Attempt,
		ContractHash: binding.ContractHash,
	}).Validate()
}

func (binding ExecutionBinding) Core() execution.Binding {
	return execution.Binding{
		PlanID:       binding.PlanID,
		TaskID:       string(binding.TaskID),
		ExecutionID:  binding.ExecutionID,
		AttemptID:    binding.AttemptID,
		StepID:       binding.StepID,
		TurnID:       binding.TurnID,
		Attempt:      binding.Attempt,
		ContractHash: binding.ContractHash,
	}
}
