package orchestration

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrTaskStateConflict        = errors.New("task state version conflict")
	ErrTaskDependencyCycle      = errors.New("task dependency would create a cycle")
	ErrTaskNotReady             = errors.New("task dependencies are not complete")
	ErrTaskClaimConflict        = errors.New("task claim conflict")
	ErrTaskVerificationRequired = errors.New("task requires verification before completion")
	ErrTaskReviewerRequired     = errors.New("task completion requires a different reviewer Agent")
	ErrTaskClaimExpired         = errors.New("task claim expired")
	ErrTaskRefinementInvalid    = errors.New("invalid task refinement")
	ErrTaskEditNotAllowed       = errors.New("task cannot be edited in its current state")
)

// TaskSnapshot is the durable projection used by schedulers and control tools.
type TaskSnapshot struct {
	ID                  TaskID            `json:"task_id"`
	TeamID              TeamID            `json:"team_id"`
	ParentTaskID        TaskID            `json:"parent_task_id,omitempty"`
	Title               string            `json:"title"`
	Description         string            `json:"description"`
	Contract            ExecutionContract `json:"contract,omitempty"`
	ContractHash        string            `json:"contract_hash,omitempty"`
	State               TaskState         `json:"state"`
	Priority            int               `json:"priority"`
	AssigneeAgentID     AgentID           `json:"assignee_agent_id,omitempty"`
	RequiredProfile     string            `json:"required_profile,omitempty"`
	VerificationProfile string            `json:"verification_profile,omitempty"`
	ClaimExpiresAt      *time.Time        `json:"claim_expires_at,omitempty"`
	Attempt             int               `json:"attempt"`
	Version             uint64            `json:"version"`
	CreatedAt           time.Time         `json:"created_at"`
	UpdatedAt           time.Time         `json:"updated_at"`
}

type TaskSpec struct {
	TeamID              TeamID
	ActorAgentID        AgentID
	ParentTaskID        TaskID
	Title               string
	Description         string
	Contract            ExecutionContract
	Priority            int
	RequiredProfile     string
	VerificationProfile string
	IdempotencyKey      string
	CorrelationID       string
}

// TaskRefinementSpec is an atomic increment to a live plan. The model may
// propose several children and their internal dependencies after learning new
// facts; the host validates and commits the whole increment together. These
// children must complete before their parent can finish. Ordinary ParentTaskID
// grouping through TaskSpec does not establish this completion dependency.
type TaskRefinementSpec struct {
	TeamID         TeamID
	ActorAgentID   AgentID
	ParentTaskID   TaskID
	Expected       uint64
	IdempotencyKey string
	CorrelationID  string
	Children       []TaskRefinementChild
	Dependencies   []TaskRefinementDependency
}

type TaskRefinementChild struct {
	Key                 string
	Title               string
	Description         string
	Contract            ExecutionContract
	Priority            int
	RequiredProfile     string
	VerificationProfile string
}

type TaskRefinementDependency struct {
	ChildKey           string
	DependencyChildKey string
	DependencyTaskID   TaskID
}

type TaskRefinementResult struct {
	ParentTaskID TaskID
	Children     map[string]TaskSnapshot
}

type TaskRefinementRepository interface {
	RefineTasks(context.Context, TaskRefinementSpec) (TaskRefinementResult, error)
}

func (spec TaskRefinementSpec) Validate() error {
	if err := spec.TeamID.Validate(); err != nil {
		return fmt.Errorf("refinement team: %w", err)
	}
	if err := spec.ParentTaskID.Validate(); err != nil {
		return fmt.Errorf("refinement parent: %w", err)
	}
	if err := spec.ActorAgentID.Validate(); err != nil {
		return fmt.Errorf("refinement actor: %w", err)
	}
	if spec.Expected == 0 || spec.Expected == AnySequence {
		return fmt.Errorf("%w: expected parent version is required", ErrTaskRefinementInvalid)
	}
	if strings.TrimSpace(spec.IdempotencyKey) == "" || len(spec.IdempotencyKey) > 512 {
		return fmt.Errorf("%w: a bounded idempotency key is required", ErrTaskRefinementInvalid)
	}
	if len(spec.Children) == 0 || len(spec.Children) > 64 {
		return fmt.Errorf("%w: children must contain 1..64 items", ErrTaskRefinementInvalid)
	}
	if len(spec.Dependencies) > 256 {
		return fmt.Errorf("%w: at most 256 dependencies are allowed", ErrTaskRefinementInvalid)
	}
	seen := make(map[string]bool, len(spec.Children))
	for _, child := range spec.Children {
		if strings.TrimSpace(child.Key) == "" || len(child.Key) > 128 || seen[child.Key] {
			return fmt.Errorf("%w: child keys must be unique non-empty values", ErrTaskRefinementInvalid)
		}
		seen[child.Key] = true
		if err := (TaskSpec{TeamID: spec.TeamID, Title: child.Title, Description: child.Description, Contract: child.Contract}).Validate(); err != nil {
			return err
		}
	}
	seenEdges := make(map[TaskRefinementDependency]bool, len(spec.Dependencies))
	for _, dependency := range spec.Dependencies {
		if !seen[dependency.ChildKey] {
			return fmt.Errorf("%w: dependency child %q is missing", ErrTaskRefinementInvalid, dependency.ChildKey)
		}
		if (dependency.DependencyChildKey == "") == (dependency.DependencyTaskID == "") {
			return fmt.Errorf("%w: dependency %q must name one child or existing task", ErrTaskRefinementInvalid, dependency.ChildKey)
		}
		if dependency.DependencyChildKey != "" && !seen[dependency.DependencyChildKey] {
			return fmt.Errorf("%w: dependency child %q is missing", ErrTaskRefinementInvalid, dependency.DependencyChildKey)
		}
		if dependency.DependencyTaskID != "" {
			if err := dependency.DependencyTaskID.Validate(); err != nil {
				return err
			}
		}
		if seenEdges[dependency] {
			return fmt.Errorf("%w: duplicate dependency", ErrTaskRefinementInvalid)
		}
		seenEdges[dependency] = true
	}
	return nil
}

func (spec TaskSpec) Validate() error {
	if err := spec.TeamID.Validate(); err != nil {
		return fmt.Errorf("task team: %w", err)
	}
	if spec.ParentTaskID != "" {
		if err := spec.ParentTaskID.Validate(); err != nil {
			return fmt.Errorf("task parent: %w", err)
		}
	}
	if spec.ActorAgentID != "" {
		if err := spec.ActorAgentID.Validate(); err != nil {
			return fmt.Errorf("task actor: %w", err)
		}
	}
	if strings.TrimSpace(spec.Title) == "" {
		return fmt.Errorf("%w: task title is required", ErrInvalidEvent)
	}
	if len(spec.Title) > 512 {
		return fmt.Errorf("%w: task title exceeds 512 bytes", ErrInvalidEvent)
	}
	if len(spec.Description) > 1024*1024 {
		return fmt.Errorf("%w: task description exceeds 1 MiB", ErrInvalidEvent)
	}
	if !spec.Contract.IsZero() {
		if err := spec.Contract.Validate(); err != nil {
			return fmt.Errorf("task contract: %w", err)
		}
	}
	return nil
}

type TaskQuery struct {
	TeamID          TeamID
	ParentTaskID    TaskID
	AssigneeAgentID AgentID
	States          []TaskState
}

type TaskTransition struct {
	TaskID          TaskID
	Expected        uint64
	Target          TaskState
	AssigneeAgentID AgentID
	ActorAgentID    AgentID
	CorrelationID   string
	Reason          string
}

// TaskUpdate edits plan fields without changing lifecycle state. Execution
// state and assignee changes must continue through TaskTransition/claim APIs.
type TaskUpdate struct {
	TaskID              TaskID
	Expected            uint64
	ActorAgentID        AgentID
	Title               *string
	Description         *string
	Priority            *int
	RequiredProfile     *string
	VerificationProfile *string
	IdempotencyKey      string
	CorrelationID       string
	Reason              string
}

type TaskRepository interface {
	CreateTask(context.Context, TaskSpec) (TaskSnapshot, error)
	GetTask(context.Context, TaskID) (TaskSnapshot, error)
	ListTasks(context.Context, TaskQuery) ([]TaskSnapshot, error)
	TransitionTask(context.Context, TaskTransition) (TaskSnapshot, error)
	ClaimTask(context.Context, TaskID, AgentID, uint64) (TaskSnapshot, error)
	ReleaseTask(context.Context, TaskID, AgentID, uint64) (TaskSnapshot, error)
	AddDependency(context.Context, TaskID, TaskID, AgentID, string) error
	RemoveDependency(context.Context, TaskID, TaskID, AgentID, string) error
	ListDependencies(context.Context, TaskID) ([]TaskID, error)
}

// TaskReassignmentRepository is an optional host capability. Implementations
// must prove the old attempt is quiescent and safe to repeat before reopening it.
type TaskReassignmentRepository interface {
	ReassignTask(context.Context, TaskID, AgentID, uint64, string) (TaskSnapshot, error)
}

// TaskPlanRepository extends plan editing without forcing read-only/task
// lifecycle adapters to implement mutation capabilities they do not own.
type TaskPlanRepository interface {
	UpdateTask(context.Context, TaskUpdate) (TaskSnapshot, error)
}

// TaskClaimReaper is implemented by durable repositories that can recover
// claims whose owner no longer has a live Agent run.
type TaskClaimReaper interface {
	ReapExpiredTaskClaims(context.Context, time.Time) (int, error)
}
