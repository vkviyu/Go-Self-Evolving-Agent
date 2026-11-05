package agentgoal

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

const AdmissionRequiredCode = "goal_admission_required"

// AdmissionGuard enforces auto admission at the existing tool boundary. It
// neither classifies user text nor grants permissions. Its turn must be bound
// before execution; the requested bit survives failed goal_start validation.
type AdmissionGuard struct {
	store     Store
	sessionID session.ID
	turnID    atomic.Pointer[string]
	requested atomic.Bool
}

func NewAdmissionGuard(store Store, sessionID session.ID, turnID string, requested bool) *AdmissionGuard {
	guard := &AdmissionGuard{store: store, sessionID: sessionID}
	guard.BindTurn(turnID)
	guard.requested.Store(requested)
	return guard
}

func (guard *AdmissionGuard) BindTurn(turnID string) { guard.turnID.Store(&turnID) }
func (guard *AdmissionGuard) Requested() bool        { return guard.requested.Load() }

func (guard *AdmissionGuard) matches(sessionID session.ID, turnID string) bool {
	bound := guard.turnID.Load()
	return bound != nil && *bound != "" && guard.sessionID == sessionID && *bound == turnID
}

func (guard *AdmissionGuard) BeforeCall(ctx context.Context, call tool.Call, descriptor tool.Descriptor) error {
	if !guard.matches(call.SessionID, call.TurnID) {
		return nil // child turns have their own admission boundary
	}
	if descriptor.Name == StartToolName {
		guard.requested.Store(true)
		return nil // allow correcting a rejected contract
	}
	requiresGoal := descriptor.Name == ReportToolName || descriptor.RiskClass == tool.RiskWrite
	// RiskExecute is not proof of a mutation (e.g. pwd, tests, diagnosis). Once
	// a task is requested, however, failed admission must not be bypassed via
	// shell or secret-bearing execution. Read/pure tools remain available.
	if guard.Requested() && (descriptor.RiskClass == tool.RiskExecute || descriptor.RiskClass == tool.RiskSecret) {
		requiresGoal = true
	}
	if !requiresGoal {
		return nil
	}
	guard.requested.Store(true)
	return guard.requireActive(ctx, call.TurnID)
}

func (guard *AdmissionGuard) BeforeComplete(ctx context.Context, sessionID session.ID, turnID string) error {
	if !guard.matches(sessionID, turnID) || !guard.Requested() {
		return nil
	}
	return guard.requireActive(ctx, turnID)
}

func (guard *AdmissionGuard) requireActive(ctx context.Context, turnID string) error {
	goal, _, found, err := guard.store.GoalForTurn(ctx, guard.sessionID, turnID)
	if err != nil {
		return fmt.Errorf("check task admission: %w", err)
	}
	if found && goal.State == StateRunning {
		return nil
	}
	return &tool.PreconditionError{
		Code:    AdmissionRequiredCode,
		Message: "This turn has no active task. Correct and successfully call goal_start before making task changes or calling goal_report. Read-only inspection is still allowed. Do not claim completion or repeat any previously successful changes.",
	}
}
