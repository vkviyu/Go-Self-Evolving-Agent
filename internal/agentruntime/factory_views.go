package agentruntime

import (
	"errors"
	"fmt"

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
	return &child, nil
}

// ActorScope is the trusted identity used for execution and Flow attribution.
func (f *Factory) ActorScope() orchestration.ActorScope {
	if f == nil {
		return orchestration.ActorScope{}
	}
	return f.config.ActorScope
}

// WithExecutionBinding narrows a factory to one immutable execution scope.
// The returned factory is independent; callers cannot mutate the parent
// runtime's binding by changing a child run.
func (f *Factory) WithExecutionBinding(binding orchestration.ExecutionBinding) (*Factory, error) {
	if f == nil {
		return nil, errors.New("Agent factory is nil")
	}
	if err := binding.Validate(); err != nil {
		return nil, fmt.Errorf("execution binding: %w", err)
	}
	child := *f
	child.config.Execution = binding
	return &child, nil
}

// WithExecutionContract attaches the immutable resource and deadline limits
// for the execution. The runtime narrows its model allowance to the contract.
func (f *Factory) WithExecutionContract(contract orchestration.ExecutionContract) (*Factory, error) {
	if f == nil {
		return nil, errors.New("Agent factory is nil")
	}
	if !contract.IsZero() {
		if err := contract.Validate(); err != nil {
			return nil, fmt.Errorf("execution contract: %w", err)
		}
	}
	child := *f
	child.config.ExecutionContract = contract
	if contract.MaxOutputTokens > 0 &&
		(child.config.MaxOutputTokens == 0 || contract.MaxOutputTokens < child.config.MaxOutputTokens) {
		child.config.MaxOutputTokens = contract.MaxOutputTokens
	}
	return &child, nil
}
