package orchestration

import "fmt"

type (
	TeamState     string
	AgentState    string
	TaskState     string
	ApprovalState string
	LeaseState    string
)

const (
	TeamActive    TeamState = "active"
	TeamDraining  TeamState = "draining"
	TeamCanceling TeamState = "canceling"
	TeamCompleted TeamState = "completed"
	TeamFailed    TeamState = "failed"
	TeamCanceled  TeamState = "canceled"
)

const (
	AgentCreated   AgentState = "created"
	AgentQueued    AgentState = "queued"
	AgentRunning   AgentState = "running"
	AgentWaiting   AgentState = "waiting"
	AgentCanceling AgentState = "canceling"
	AgentCompleted AgentState = "completed"
	AgentFailed    AgentState = "failed"
	AgentCanceled  AgentState = "canceled"
)

const (
	TaskDraft        TaskState = "draft"
	TaskBlocked      TaskState = "blocked"
	TaskReady        TaskState = "ready"
	TaskClaimed      TaskState = "claimed"
	TaskInProgress   TaskState = "in_progress"
	TaskVerification TaskState = "verification"
	// TaskAwaitingMerge holds finished snapshot work until its candidate is
	// committed to the main workspace, so dependents never start early.
	TaskAwaitingMerge TaskState = "awaiting_merge"
	TaskCompleted     TaskState = "completed"
	TaskFailed        TaskState = "failed"
	TaskCanceled      TaskState = "canceled"
)

const (
	ApprovalRequested ApprovalState = "requested"
	ApprovalApproved  ApprovalState = "approved"
	ApprovalDenied    ApprovalState = "denied"
	ApprovalExpired   ApprovalState = "expired"
	ApprovalCanceled  ApprovalState = "canceled"
)

const (
	LeaseRequested  LeaseState = "requested"
	LeaseActive     LeaseState = "active"
	LeaseReleased   LeaseState = "released"
	LeaseExpired    LeaseState = "expired"
	LeaseConflicted LeaseState = "conflicted"
	LeaseRejected   LeaseState = "rejected"
	// LeaseAbandoned marks a lease reclaimed after its holder instance and
	// every descendant process it started had exited.
	LeaseAbandoned LeaseState = "abandoned"
)

// ValidateTeamTransition reports whether a Team may move from one state to
// another. A session's Team outlives each of its runs, so a completed, failed
// or canceled Team becomes active again when its session is reopened.
func ValidateTeamTransition(from, to TeamState) error {
	return validateTransition("team", string(from), string(to), map[TeamState]map[TeamState]bool{
		TeamActive:    {TeamDraining: true, TeamCanceling: true},
		TeamDraining:  {TeamCompleted: true, TeamFailed: true, TeamCanceling: true},
		TeamCanceling: {TeamCanceled: true},
		TeamCompleted: {TeamActive: true},
		TeamFailed:    {TeamActive: true},
		TeamCanceled:  {TeamActive: true},
	})
}

// ValidateAgentTransition reports whether an Agent may move from one state to
// another. Completed and failed Agents may be queued again for a new
// assignment; canceled Agents are terminal.
func ValidateAgentTransition(from, to AgentState) error {
	return validateTransition("agent", string(from), string(to), map[AgentState]map[AgentState]bool{
		AgentCreated:   {AgentQueued: true, AgentCanceled: true},
		AgentQueued:    {AgentRunning: true, AgentCanceling: true, AgentFailed: true},
		AgentRunning:   {AgentQueued: true, AgentWaiting: true, AgentCompleted: true, AgentFailed: true, AgentCanceling: true},
		AgentWaiting:   {AgentQueued: true, AgentCanceling: true, AgentFailed: true},
		AgentCanceling: {AgentCanceled: true, AgentFailed: true},
		AgentCompleted: {AgentQueued: true},
		AgentFailed:    {AgentQueued: true},
	})
}

// ValidateTaskTransition reports whether a task may move from one state to
// another. Completed, failed and canceled tasks are terminal.
func ValidateTaskTransition(from, to TaskState) error {
	return validateTransition("task", string(from), string(to), map[TaskState]map[TaskState]bool{
		TaskDraft:         {TaskBlocked: true, TaskReady: true, TaskCanceled: true},
		TaskBlocked:       {TaskReady: true, TaskFailed: true, TaskCanceled: true},
		TaskReady:         {TaskBlocked: true, TaskClaimed: true, TaskCanceled: true},
		TaskClaimed:       {TaskBlocked: true, TaskReady: true, TaskInProgress: true, TaskFailed: true, TaskCanceled: true},
		TaskInProgress:    {TaskBlocked: true, TaskReady: true, TaskVerification: true, TaskAwaitingMerge: true, TaskCompleted: true, TaskFailed: true, TaskCanceled: true},
		TaskVerification:  {TaskBlocked: true, TaskReady: true, TaskAwaitingMerge: true, TaskCompleted: true, TaskFailed: true, TaskCanceled: true},
		TaskAwaitingMerge: {TaskBlocked: true, TaskCompleted: true, TaskFailed: true, TaskCanceled: true},
	})
}

// validateTransition checks from -> to against the transition table of domain.
func validateTransition[S ~string](domain, from, to string, transitions map[S]map[S]bool) error {
	if from == "" || to == "" {
		return fmt.Errorf("%s transition requires non-empty states", domain)
	}
	if from == to || !transitions[S(from)][S(to)] {
		return fmt.Errorf("invalid %s transition %q -> %q", domain, from, to)
	}
	return nil
}
