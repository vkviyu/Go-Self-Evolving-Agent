package agentflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

type RuntimeConfig struct {
	Pipeline *tool.Pipeline
	Registry *tool.Registry
	Store    Store
}

type Runtime struct {
	pipeline *tool.Pipeline
	registry *tool.Registry
	store    Store
	runs     *runCoordinator
}

func NewRuntime(config RuntimeConfig) (*Runtime, error) {
	if config.Pipeline == nil || config.Registry == nil || config.Store == nil {
		return nil, errors.New("Flow runtime requires pipeline, registry and durable store")
	}
	return &Runtime{pipeline: config.Pipeline, registry: config.Registry, store: config.Store, runs: newRunCoordinator()}, nil
}

type Invocation struct {
	Actor              orchestration.ActorScope
	SessionID          session.ID
	IdempotencyKey     string
	Input              Value
	Pipeline           *tool.Pipeline
	RegistryGeneration uint64
	RuntimeSnapshotID  string
	Execution          orchestration.ExecutionBinding
}

type Result struct {
	Run     Run
	Outputs []Value
}

func (runtime *Runtime) Run(ctx context.Context, definition *Definition, invocation Invocation) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if definition == nil {
		return Result{}, errors.New("Flow definition is nil")
	}
	if err := definition.Validate(); err != nil {
		return Result{}, err
	}
	fingerprint, err := definition.Fingerprint()
	if err != nil {
		return Result{}, err
	}
	if err := invocation.Actor.Validate(); err != nil {
		return Result{}, fmt.Errorf("Flow actor: %w", err)
	}
	if invocation.SessionID == "" || strings.TrimSpace(invocation.IdempotencyKey) == "" {
		return Result{}, errors.New("Flow invocation requires session and idempotency key")
	}
	if err := invocation.Input.Validate(); err != nil {
		return Result{}, err
	}
	runKey := string(invocation.Actor.TeamID) + "\x00" + string(invocation.SessionID) + "\x00" + invocation.IdempotencyKey
	if active, _ := ctx.Value(activeFlowInvocationKey{}).(string); active == runKey {
		return Result{}, fmt.Errorf("%w: %s", ErrRecursiveInvocation, invocation.IdempotencyKey)
	}
	unlockRun, err := runtime.runs.lock(ctx, runKey)
	if err != nil {
		return Result{}, err
	}
	defer unlockRun()
	ctx = context.WithValue(ctx, activeFlowInvocationKey{}, runKey)
	runID, err := NewID()
	if err != nil {
		return Result{}, err
	}
	snapshot := runtime.registry.Snapshot()
	if invocation.RuntimeSnapshotID != "" {
		snapshot, err = runtime.registry.ResolveSnapshot(ctx, invocation.RuntimeSnapshotID)
		if err != nil {
			return Result{}, err
		}
	} else if invocation.RegistryGeneration != 0 {
		var ok bool
		snapshot, ok = runtime.registry.SnapshotAt(invocation.RegistryGeneration)
		if !ok {
			return Result{}, fmt.Errorf("Flow caller registry snapshot %d is unavailable", invocation.RegistryGeneration)
		}
	}
	snapshotID, err := snapshot.ID()
	if err != nil {
		return Result{}, err
	}
	if err := runtime.registry.RetainSnapshot(ctx, snapshot, tool.SnapshotReference{Kind: "flow", Scope: string(invocation.Actor.TeamID), ID: invocation.IdempotencyKey}); err != nil {
		if errors.Is(err, tool.ErrSnapshotReferenceConflict) {
			return Result{}, errors.Join(orchestration.ErrIdempotencyConflict, err)
		}
		return Result{}, fmt.Errorf("retain Flow runtime: %w", err)
	}
	run, err := runtime.store.CreateRun(ctx, RunRequest{
		ID: runID, IdempotencyKey: invocation.IdempotencyKey, Definition: definition.Name,
		DefinitionRevision: definition.Revision, DefinitionFingerprint: fingerprint,
		SessionID: invocation.SessionID, TeamID: invocation.Actor.TeamID, AgentID: invocation.Actor.AgentID,
		Input: invocation.Input, RegistryGeneration: snapshot.Generation(), RuntimeSnapshotID: snapshotID,
		Execution: invocation.Execution,
	})
	if err != nil {
		return Result{}, err
	}
	if run.Definition != definition.Name || run.DefinitionRevision != definition.Revision || run.DefinitionFingerprint != fingerprint {
		return Result{Run: run}, fmt.Errorf("%w: durable %s@%s/%s, requested %s@%s/%s",
			ErrDefinitionMismatch, run.Definition, run.DefinitionRevision, run.DefinitionFingerprint,
			definition.Name, definition.Revision, fingerprint)
	}
	if run.State == StateCompleted {
		return Result{Run: run, Outputs: cloneValues(run.Outputs)}, nil
	}
	if run.State == StateFailed || run.State == StateCanceled {
		return Result{Run: run}, fmt.Errorf("Flow %s is %s: %s", run.ID, run.State, run.Error)
	}
	snapshot, err = runtime.registry.ResolveSnapshot(ctx, run.RuntimeSnapshotID)
	if err != nil {
		return Result{}, fmt.Errorf("restore Flow: %w", err)
	}
	pipeline := invocation.Pipeline
	if pipeline == nil {
		pipeline = runtime.pipeline
	}
	execution := &Execution{
		runtime: runtime, run: run, actor: invocation.Actor, sessionID: invocation.SessionID,
		pipeline: pipeline, snapshot: snapshot, runtimeSnapshotID: run.RuntimeSnapshotID,
	}
	runContext, cancel := context.WithCancel(ctx)
	defer cancel()
	stream := singleton(invocation.Input)
	for _, stage := range definition.Stages {
		stream = stage.Apply(runContext, execution, stream)
	}
	outputs := make([]Value, 0)
	for item := range stream {
		if item.Err != nil {
			if _, ok := errors.AsType[*orchestration.WaitSuspendedError](item.Err); ok {
				current, loadErr := runtime.store.GetRun(context.WithoutCancel(ctx), run.ID)
				if loadErr != nil {
					return Result{Run: run}, errors.Join(item.Err, loadErr)
				}
				return Result{Run: current}, item.Err
			}
			state := StateFailed
			if errors.Is(item.Err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
				state = StateCanceled
			}
			finished, finishErr := runtime.store.FinishRun(context.WithoutCancel(ctx), run.ID, state, nil, item.Err.Error())
			return Result{Run: finished}, errors.Join(item.Err, finishErr)
		}
		if err := item.Value.Validate(); err != nil {
			finished, finishErr := runtime.store.FinishRun(context.WithoutCancel(ctx), run.ID, StateFailed, nil, err.Error())
			return Result{Run: finished}, errors.Join(err, finishErr)
		}
		outputs = append(outputs, item.Value.Clone())
		if item.Final {
			cancel()
			break
		}
	}
	// A stage is allowed to stop by observing runContext cancellation without
	// emitting an error. Parent cancellation must still win over any partial
	// outputs collected before that quiet shutdown.
	if ctx.Err() != nil {
		finished, finishErr := runtime.store.FinishRun(context.WithoutCancel(ctx), run.ID, StateCanceled, nil, ctx.Err().Error())
		return Result{Run: finished}, errors.Join(ctx.Err(), finishErr)
	}
	if len(outputs) == 0 {
		finished, finishErr := runtime.store.FinishRun(context.WithoutCancel(ctx), run.ID, StateFailed, nil, ErrNoResults.Error())
		return Result{Run: finished}, errors.Join(ErrNoResults, finishErr)
	}
	finished, err := runtime.store.FinishRun(context.WithoutCancel(ctx), run.ID, StateCompleted, outputs, "")
	if err != nil {
		return Result{}, err
	}
	return Result{Run: finished, Outputs: cloneValues(outputs)}, nil
}

type activeFlowInvocationKey struct{}

type Execution struct {
	runtime           *Runtime
	run               Run
	actor             orchestration.ActorScope
	sessionID         session.ID
	pipeline          *tool.Pipeline
	snapshot          *tool.Snapshot
	runtimeSnapshotID string
}

// Do starts a step against its input hash and reuses the stored outputs if the step
// already completed.
func (execution *Execution) Do(ctx context.Context, stepID string, input Value, action func(context.Context) ([]Value, error)) ([]Value, error) {
	if err := validName("Flow step", stepID); err != nil {
		return nil, err
	}
	hash, err := valueHash(input)
	if err != nil {
		return nil, err
	}
	step, err := execution.runtime.store.StartStep(ctx, execution.run.ID, stepID, hash)
	if err != nil {
		return nil, err
	}
	if step.State == StepCompleted {
		return cloneValues(step.Outputs), nil
	}
	outputs, err := action(ctx)
	if err != nil {
		if _, ok := errors.AsType[*orchestration.WaitSuspendedError](err); ok {
			return validValues(outputs), err
		}
		_ = execution.runtime.store.FailStep(context.WithoutCancel(ctx), execution.run.ID, stepID, err.Error())
		return validValues(outputs), err
	}
	for _, output := range outputs {
		if err := output.Validate(); err != nil {
			_ = execution.runtime.store.FailStep(context.WithoutCancel(ctx), execution.run.ID, stepID, err.Error())
			return nil, err
		}
	}
	step, err = execution.runtime.store.FinishStep(context.WithoutCancel(ctx), execution.run.ID, stepID, outputs)
	if err != nil {
		return nil, err
	}
	return cloneValues(step.Outputs), nil
}

// Tool executes against the run's pinned tool snapshot. The run and step IDs form the tool
// call ID.
func (execution *Execution) Tool(ctx context.Context, stepID, toolName string, arguments any) (json.RawMessage, error) {
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return nil, err
	}
	callID := execution.run.ID + ":" + stepID
	binding := execution.run.Execution
	if !binding.IsZero() {
		binding.StepID = stepID
		binding.TurnID = "flow:" + execution.run.ID
		if err := binding.Validate(); err != nil {
			return nil, fmt.Errorf("Flow step execution binding: %w", err)
		}
	}
	result, err := execution.pipeline.Execute(ctx, tool.Call{
		Actor: execution.actor, SessionID: execution.sessionID, TurnID: "flow:" + execution.run.ID,
		Execution:         binding,
		RuntimeSnapshotID: execution.runtimeSnapshotID, CallID: callID,
		ToolName: toolName, Arguments: encoded, Registry: execution.snapshot,
	})
	if err != nil {
		return nil, err
	}
	return append(json.RawMessage(nil), result.Data...), nil
}

func validValues(values []Value) []Value {
	valid := make([]Value, 0, len(values))
	for _, value := range values {
		if value.Validate() == nil {
			valid = append(valid, value.Clone())
		}
	}
	return valid
}

func (execution *Execution) sessionName(stageName, key string) string {
	name := stageName
	if key != "" {
		digest := sha256.Sum256([]byte(key))
		name += "." + hex.EncodeToString(digest[:6])
	}
	return name
}

func singleton(value Value) Stream {
	stream := make(chan Item, 1)
	// The source identity is part of the durable step key. Keeping it
	// separate from the value means two equal payloads can still represent
	// distinct work items once a custom stage fans them out.
	stream <- Item{Value: value.Clone(), identity: "root"}
	close(stream)
	return stream
}

func cloneValues(values []Value) []Value {
	clones := make([]Value, len(values))
	for index := range values {
		clones[index] = values[index].Clone()
	}
	return clones
}

// runCoordinator serializes duplicate idempotent Flow invocations within one
// process. The durable run/step checkpoints still provide crash recovery; the
// process-local lock prevents two callers from concurrently executing the same
// running run and duplicating pure callbacks or racing session-stage writes.
type runCoordinator struct {
	mu    sync.Mutex
	locks map[string]*runLock
}

type runLock struct {
	token chan struct{}
	refs  int
}

func newRunCoordinator() *runCoordinator {
	return &runCoordinator{locks: make(map[string]*runLock)}
}

func (coordinator *runCoordinator) lock(ctx context.Context, key string) (func(), error) {
	coordinator.mu.Lock()
	entry := coordinator.locks[key]
	if entry == nil {
		entry = &runLock{token: make(chan struct{}, 1)}
		coordinator.locks[key] = entry
	}
	entry.refs++
	coordinator.mu.Unlock()
	select {
	case entry.token <- struct{}{}:
	case <-ctx.Done():
		coordinator.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(coordinator.locks, key)
		}
		coordinator.mu.Unlock()
		return nil, ctx.Err()
	}
	return func() {
		<-entry.token
		coordinator.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(coordinator.locks, key)
		}
		coordinator.mu.Unlock()
	}, nil
}
