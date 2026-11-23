package execution

import (
	"errors"
	"fmt"
	"time"
)

type Entity string

const (
	EntityPlan      Entity = "plan"
	EntityExecution Entity = "execution"
	EntityStep      Entity = "step"
)

type State string

const (
	PlanDraft     State = "draft"
	PlanAdmitted  State = "admitted"
	PlanRunning   State = "running"
	PlanCompleted State = "completed"
	PlanFailed    State = "failed"
	PlanCanceled  State = "canceled"

	ExecutionReserved   State = "reserved"
	ExecutionAdmitted   State = "admitted"
	ExecutionRunning    State = "running"
	ExecutionWaiting    State = "waiting"
	ExecutionVerifying  State = "verifying"
	ExecutionDelivering State = "delivering"
	ExecutionPaused     State = "paused_unknown"
	ExecutionSucceeded  State = "completed"
	ExecutionFailed     State = "failed"
	ExecutionCanceled   State = "canceled"

	StepPending   State = "pending"
	StepAdmitted  State = "admitted"
	StepRunning   State = "running"
	StepWaiting   State = "waiting"
	StepCompleted State = "completed"
	StepUnknown   State = "unknown"
	StepFailed    State = "failed"
)

var ErrInvalidTransition = errors.New("invalid execution lifecycle transition")

func CanTransition(entity Entity, from, to State) bool {
	if from == to {
		return true
	}
	switch entity {
	case EntityPlan:
		return transition(from, to,
			PlanDraft, PlanAdmitted,
			PlanAdmitted, PlanRunning,
			PlanRunning, PlanCompleted,
			PlanRunning, PlanFailed,
			PlanRunning, PlanCanceled,
		)
	case EntityExecution:
		return transition(from, to,
			ExecutionReserved, ExecutionAdmitted,
			ExecutionAdmitted, ExecutionRunning,
			ExecutionRunning, ExecutionWaiting,
			ExecutionWaiting, ExecutionRunning,
			ExecutionRunning, ExecutionVerifying,
			ExecutionVerifying, ExecutionDelivering,
			ExecutionDelivering, ExecutionSucceeded,
			ExecutionRunning, ExecutionPaused,
			ExecutionWaiting, ExecutionPaused,
			ExecutionVerifying, ExecutionPaused,
			ExecutionDelivering, ExecutionPaused,
			ExecutionRunning, ExecutionFailed,
			ExecutionWaiting, ExecutionFailed,
			ExecutionVerifying, ExecutionFailed,
			ExecutionDelivering, ExecutionFailed,
			ExecutionReserved, ExecutionCanceled,
			ExecutionAdmitted, ExecutionCanceled,
			ExecutionRunning, ExecutionCanceled,
			ExecutionWaiting, ExecutionCanceled,
			ExecutionPaused, ExecutionAdmitted,
		)
	case EntityStep:
		return transition(from, to,
			StepPending, StepAdmitted,
			StepAdmitted, StepRunning,
			StepRunning, StepWaiting,
			StepWaiting, StepRunning,
			StepRunning, StepCompleted,
			StepRunning, StepUnknown,
			StepWaiting, StepUnknown,
			StepRunning, StepFailed,
			StepWaiting, StepFailed,
		)
	default:
		return false
	}
}

func Transition(entity Entity, from, to State) error {
	if !CanTransition(entity, from, to) {
		return fmt.Errorf("%w: %s %q → %q", ErrInvalidTransition, entity, from, to)
	}
	return nil
}

func transition(from, to State, pairs ...State) bool {
	for index := 0; index+1 < len(pairs); index += 2 {
		if pairs[index] == from && pairs[index+1] == to {
			return true
		}
	}
	return false
}

type Plan struct {
	ID           string    `json:"plan_id"`
	Contract     Contract  `json:"contract"`
	ContractHash string    `json:"contract_hash"`
	State        State     `json:"state"`
	Version      uint64    `json:"version"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type Execution struct {
	ID           string     `json:"execution_id"`
	PlanID       string     `json:"plan_id,omitempty"`
	TaskID       string     `json:"task_id,omitempty"`
	ContractHash string     `json:"contract_hash"`
	Attempt      int        `json:"attempt"`
	State        State      `json:"state"`
	Version      uint64     `json:"version"`
	StartedAt    time.Time  `json:"started_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	Deadline     *time.Time `json:"deadline,omitempty"`
}

type Step struct {
	ID           string    `json:"step_id"`
	ExecutionID  string    `json:"execution_id"`
	ContractHash string    `json:"contract_hash"`
	Attempt      int       `json:"attempt"`
	State        State     `json:"state"`
	Version      uint64    `json:"version"`
	StartedAt    time.Time `json:"started_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}
