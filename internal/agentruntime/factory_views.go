package agentruntime

import (
	"errors"
	"fmt"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentgoal"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenizer"
)

// WithActorScope creates a child-scoped factory while retaining the same
// provider and stores. The tokenizer is forked and calibration starts fresh,
// because provider-reported prompt sizes of one Agent say nothing about
// another Agent's prompt framing. The original factory remains immutable.
func (f *Factory) WithActorScope(scope orchestration.ActorScope) (*Factory, error) {
	if f == nil {
		return nil, errors.New("Agent factory is nil")
	}
	if err := scope.Validate(); err != nil {
		return nil, fmt.Errorf("child factory actor scope: %w", err)
	}
	child := *f
	child.config.ActorScope = scope
	child.mailbox = nil
	child.tokenizer = tokenizer.Fork(f.tokenizer)
	child.config.Tokenizer = child.tokenizer
	child.calibration = &contextCalibration{}
	if child.goalMode.Enabled {
		var err error
		child.goalEval, err = child.GoalEvaluator(child.goalMode.Assessment)
		if err != nil {
			return nil, err
		}
	}
	return &child, nil
}

// GoalRuntime returns the immutable Outer Loop dependencies shared by root,
// delegated and Flow-invoked Agent sessions.
func (f *Factory) GoalRuntime() (agentgoal.Config, agentgoal.Store, agentgoal.Evaluator, agentgoal.CapGuard) {
	if f == nil {
		return agentgoal.Config{}, nil, nil, agentgoal.CapGuard{}
	}
	return f.goalMode, f.goalStore, f.goalEval, agentgoal.CapGuard{Budget: f.goalBudget}
}

// GoalRuntimeStore exposes the durable Goal port for optional lifecycle
// extensions such as AdmissionRepository.
func (f *Factory) GoalRuntimeStore() agentgoal.Store {
	if f == nil {
		return nil
	}
	return f.goalStore
}

// WithoutGoalMode returns an immutable view for a direct conversation turn.
// Registry, policy, accounting and history remain shared; only the goal
// protocol is disabled. The next task can use the original factory again.
func (f *Factory) WithoutGoalMode() *Factory {
	direct := *f
	direct.goalMode.Enabled = false
	direct.config.GoalMode.Enabled = false
	return &direct
}

// WithActiveGoal disables new admission while preserving the reporting protocol.
func (f *Factory) WithActiveGoal() *Factory {
	active := *f
	active.goalMode.Enabled = true
	active.goalMode.Activation = agentgoal.ActivationAlways
	return &active
}

// GoalEvaluator resolves the persisted assessment strategy, not today's default.
func (f *Factory) GoalEvaluator(strategy agentgoal.AssessmentStrategy) (agentgoal.Evaluator, error) {
	independent := f.config.GoalEvaluator
	if independent == nil && strategy != agentgoal.AssessmentSelf {
		independent = &modelGoalEvaluator{factory: f}
	}
	if strategy == agentgoal.AssessmentAdaptive {
		return agentgoal.AdaptiveEvaluator{Verifier: &fileGoalVerifier{factory: f}, Fallback: independent}, nil
	}
	return agentgoal.Compose(strategy, independent)
}

// ActorScope is the trusted identity used for Goal and Flow attribution.
func (f *Factory) ActorScope() orchestration.ActorScope {
	if f == nil {
		return orchestration.ActorScope{}
	}
	return f.config.ActorScope
}
