package tool

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/policy"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

var (
	ErrPolicyDenied   = errors.New("tool call denied by policy")
	ErrApprovalDenied = errors.New("tool call approval denied")
	ErrCallConflict   = errors.New("tool call ID conflicts with a durable invocation")
)

// ExecutionError reports a tool-domain failure that was durably recorded.
// Adapters may safely return it to the model as a tool result so the Agent can
// correct its arguments. Infrastructure and durability failures remain fatal.
type ExecutionError struct {
	Tool string
	Err  error
}

func (err *ExecutionError) Error() string {
	if err == nil {
		return "tool execution failed"
	}
	return fmt.Sprintf("invoke %s: %v", err.Tool, err.Err)
}

func (err *ExecutionError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.Err
}

// Call is a tool invocation bound to an immutable turn snapshot.
type Call struct {
	Actor             orchestration.ActorScope
	Execution         orchestration.ExecutionBinding
	SessionID         session.ID
	TurnID            string
	RuntimeSnapshotID string
	CallID            string
	ToolName          string
	Arguments         json.RawMessage
	Registry          *Snapshot
}

// Result is the validated JSON result of a completed tool call.
type Result struct {
	CallID   string
	Revision string
	Data     json.RawMessage
}

// Pipeline centralizes recording, policy and execution for every tool kind.
type Pipeline struct {
	registry          *Registry
	policy            policy.Engine
	approver          policy.Approver
	events            session.EventStore
	calls             *callCoordinator
	guards            []ExecutionGuard
	durabilityTimeout time.Duration
}

// PipelineConfig contains the trusted services required by Pipeline. Keeping
// construction in one place makes an invalid execution boundary impossible in
// production code while preserving the small dependency surface for tests.
type PipelineConfig struct {
	Registry *Registry
	Policy   policy.Engine
	Approver policy.Approver
	Events   session.EventStore
	// DurabilityTimeout bounds detached outcome and audit writes. The host
	// supplies its storage batch budget, including startup and lock contention.
	// Zero selects 30 seconds.
	DurabilityTimeout time.Duration
}

// NewPipeline validates and constructs the single tool execution boundary.
func NewPipeline(config PipelineConfig) (*Pipeline, error) {
	if config.DurabilityTimeout < 0 {
		return nil, errors.New("tool durability timeout must be positive")
	}
	if config.DurabilityTimeout == 0 {
		config.DurabilityTimeout = 30 * time.Second
	}
	pipeline := &Pipeline{
		registry:          config.Registry,
		policy:            config.Policy,
		approver:          config.Approver,
		events:            config.Events,
		calls:             newCallCoordinator(),
		guards:            []ExecutionGuard{ExecutionBoundary{}},
		durabilityTimeout: config.DurabilityTimeout,
	}
	if err := pipeline.validate(); err != nil {
		return nil, err
	}
	return pipeline, nil
}

// WithApprover returns an independent pipeline view for a caller's execution
// mode. The registry, policy and event store remain shared; the approver is the
// only mode-specific concern (interactive UI versus fail-closed headless mode).
func (p *Pipeline) WithApprover(approver policy.Approver) *Pipeline {
	if p == nil {
		return nil
	}
	copyPipeline := *p
	copyPipeline.approver = approver
	return &copyPipeline
}

// ExecutionGuard is a trusted caller restriction, applied to nested Flow
// actions as well as model-visible tools. BeforeStart may serialize admission
// until tool.started is durable; its release runs before the executor (which
// can recursively call this pipeline).
type ExecutionGuard interface {
	CheckCall(context.Context, Call, Descriptor) error
	BeforeStart(context.Context, Call, Descriptor) (func(), error)
}

func (p *Pipeline) WithExecutionGuard(guard ExecutionGuard) *Pipeline {
	copyPipeline := *p
	copyPipeline.guards = append(append([]ExecutionGuard(nil), p.guards...), guard)
	return &copyPipeline
}

// Execute resolves exactly one revision and records the durable call boundaries.
func (p *Pipeline) Execute(ctx context.Context, call Call) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := p.validate(); err != nil {
		return Result{}, err
	}
	if err := validateCall(&call); err != nil {
		return Result{}, err
	}
	snapshot, binding, err := p.resolve(call)
	if err != nil {
		return Result{}, err
	}
	for _, guard := range p.guards {
		if err := guard.CheckCall(ctx, call, binding.Descriptor); err != nil {
			return Result{}, err
		}
	}
	unlock := p.calls.lock(string(call.SessionID) + "\x00" + call.TurnID + "\x00" + call.CallID)
	defer unlock()
	if replayed, found, err := p.replayOutcome(ctx, call, binding); err != nil {
		return Result{}, err
	} else if found {
		return replayed, nil
	}
	var resumedWait bool
	ctx, resumedWait, err = p.restoreWaitIntent(ctx, call, binding)
	if err != nil {
		return Result{}, err
	}
	if resumedWait {
		// This is the continuation of an already admitted invocation, not
		// another tool call. Re-running admission would charge its tool quota
		// twice and can make a valid wait impossible to resume at the limit.
		return p.invokeAndRecord(ctx, call, binding, snapshot)
	}
	if err := p.recordProposal(ctx, call, binding, snapshot); err != nil {
		return Result{}, err
	}
	if err := snapshot.validateArguments(binding, call.Arguments); err != nil {
		if recordErr := p.recordFailure(ctx, call, binding, err); recordErr != nil {
			return Result{}, recordErr
		}
		return Result{}, err
	}
	if err := checkCallPreconditions(ctx, call, binding.Descriptor); err != nil {
		if recordErr := p.recordFailure(ctx, call, binding, err); recordErr != nil {
			return Result{}, recordErr // durability failures must remain fatal
		}
		return Result{}, err
	}
	request := makePolicyRequest(call, binding)
	decision, err := p.evaluate(ctx, call, request)
	if err != nil {
		return Result{}, err
	}
	if err := p.authorize(ctx, call, binding, request, decision); err != nil {
		return Result{}, err
	}
	if err := p.admitStart(ctx, call, binding); err != nil {
		return Result{}, err
	}
	// Receipt reconciliation may outlive the caller's cancellation. A durable
	// start record grants auditability, never permission to execute afterward.
	if err := ctx.Err(); err != nil {
		return Result{}, errors.Join(err, p.recordFailure(ctx, call, binding, err))
	}
	return p.invokeAndRecord(ctx, call, binding, snapshot)
}

func (p *Pipeline) invokeAndRecord(ctx context.Context, call Call, binding Binding, snapshot *Snapshot) (Result, error) {
	data, err := p.invoke(ctx, call, binding, snapshot)
	if err != nil {
		return Result{}, err
	}
	if err := p.recordSuccess(ctx, call, binding, data); err != nil {
		return Result{}, err
	}
	return Result{CallID: call.CallID, Revision: binding.Descriptor.Revision, Data: data}, nil
}

func (p *Pipeline) admitStart(ctx context.Context, call Call, binding Binding) error {
	for _, guard := range p.guards {
		release, err := guard.BeforeStart(ctx, call, binding.Descriptor)
		if err != nil {
			return err
		}
		if release != nil {
			defer release()
		}
	}
	return p.recordStart(ctx, call, binding)
}

func validateCall(call *Call) error {
	if call.SessionID == "" || call.ToolName == "" {
		return errors.New("tool call requires session ID and tool name")
	}
	if len(call.Arguments) > maxArgumentBytes {
		return &PreconditionError{Code: "invalid_arguments", Message: "tool arguments exceed the size limit"}
	}
	if len(call.Arguments) == 0 || !json.Valid(call.Arguments) {
		return errors.New("tool arguments must be valid JSON")
	}
	if !call.Actor.IsZero() {
		if err := call.Actor.Validate(); err != nil {
			return fmt.Errorf("invalid tool actor scope: %w", err)
		}
	}
	if call.CallID != "" {
		return nil
	}
	var err error
	call.CallID, err = callID()
	return err
}

func (p *Pipeline) resolve(call Call) (*Snapshot, Binding, error) {
	snapshot := call.Registry
	if snapshot == nil {
		snapshot = p.registry.Snapshot()
	} else if !p.registry.OwnsSnapshot(snapshot) {
		return nil, Binding{}, ErrForeignSnapshot
	}
	binding, ok := snapshot.Get(call.ToolName)
	if !ok {
		return nil, Binding{}, fmt.Errorf("%w: %s", ErrToolNotFound, call.ToolName)
	}
	return snapshot, binding, nil
}

func (p *Pipeline) recordProposal(ctx context.Context, call Call, binding Binding, snapshot *Snapshot) error {
	_, err := p.events.Append(ctx, call.SessionID, session.AnySequence, session.PendingEvent{
		Type:              session.EventToolProposed,
		CorrelationID:     call.CallID,
		RuntimeSnapshotID: call.RuntimeSnapshotID,
		Payload: session.MustJSONPayload(map[string]any{
			"actor":               call.Actor,
			"execution":           call.Execution,
			"turn_id":             call.TurnID,
			"call_id":             call.CallID,
			"tool":                binding.Descriptor.Name,
			"revision":            binding.Descriptor.Revision,
			"risk_class":          binding.Descriptor.RiskClass,
			"registry_generation": snapshot.Generation(),
			"arguments":           json.RawMessage(call.Arguments),
		}),
	})
	if err != nil {
		return fmt.Errorf("record proposed tool call: %w", err)
	}
	emitLiveEvent(ctx, LiveEvent{
		Type: LiveToolProposed, CallID: call.CallID, TurnID: call.TurnID,
		ToolName: binding.Descriptor.Name,
	})
	return nil
}

func makePolicyRequest(call Call, binding Binding) policy.Request {
	return policy.Request{
		Actor:         call.Actor,
		Execution:     call.Execution,
		CallID:        call.CallID,
		SessionID:     string(call.SessionID),
		ToolName:      binding.Descriptor.Name,
		Revision:      binding.Descriptor.Revision,
		Capability:    "tool.invoke",
		Resource:      "tool:" + binding.Descriptor.Name,
		RiskClass:     string(binding.Descriptor.RiskClass),
		NormalizedArg: append(json.RawMessage(nil), call.Arguments...),
	}
}

func (p *Pipeline) evaluate(ctx context.Context, call Call, request policy.Request) (policy.Decision, error) {
	emitLiveEvent(ctx, LiveEvent{
		Type: LivePolicyChecking, CallID: call.CallID, TurnID: call.TurnID,
		ToolName: request.ToolName,
	})
	decision, err := p.policy.Evaluate(ctx, request)
	if err != nil {
		return policy.Decision{}, fmt.Errorf("evaluate tool policy: %w", err)
	}
	err = p.recordAudit(ctx, call, session.PendingEvent{
		Type:              session.EventPolicyDecided,
		CorrelationID:     call.CallID,
		RuntimeSnapshotID: call.RuntimeSnapshotID,
		Payload: session.MustJSONPayload(map[string]any{
			"actor":   call.Actor,
			"turn_id": call.TurnID,
			"call_id": call.CallID,
			"effect":  decision.Effect,
			"rule_id": decision.RuleID,
			"reason":  decision.Reason,
		}),
	})
	if err != nil {
		return policy.Decision{}, fmt.Errorf("record policy decision: %w", err)
	}
	emitLiveEvent(ctx, LiveEvent{
		Type: LivePolicyDecided, CallID: call.CallID, TurnID: call.TurnID,
		ToolName: request.ToolName,
	})
	return decision, nil
}

func (p *Pipeline) authorize(ctx context.Context, call Call, binding Binding, request policy.Request, decision policy.Decision) error {
	switch decision.Effect {
	case policy.EffectAllow:
		return nil
	case policy.EffectDeny:
		denied := fmt.Errorf("%w: %s", ErrPolicyDenied, decision.Reason)
		return errors.Join(denied, p.recordFailure(ctx, call, binding, denied))
	case policy.EffectRequireApproval:
		if p.approver == nil {
			unavailable := policy.ErrApprovalUnavailable
			return errors.Join(unavailable, p.recordFailure(ctx, call, binding, unavailable))
		}
		if err := p.recordAudit(ctx, call, session.PendingEvent{
			Type:              session.EventApprovalRequested,
			CorrelationID:     call.CallID,
			RuntimeSnapshotID: call.RuntimeSnapshotID,
			Payload:           session.MustJSONPayload(map[string]any{"actor": call.Actor, "turn_id": call.TurnID, "call_id": call.CallID, "rule_id": decision.RuleID}),
		}); err != nil {
			return fmt.Errorf("record approval request: %w", err)
		}
		emitLiveEvent(ctx, LiveEvent{
			Type: LiveApprovalRequested, CallID: call.CallID, TurnID: call.TurnID,
			ToolName: request.ToolName,
		})
		emitLiveEvent(ctx, LiveEvent{
			Type: LiveApprovalWaiting, CallID: call.CallID, TurnID: call.TurnID,
			ToolName: request.ToolName,
		})
		approved, err := p.approver.Approve(ctx, request, decision)
		if err != nil {
			return errors.Join(fmt.Errorf("resolve tool approval: %w", err), p.recordFailure(ctx, call, binding, err))
		}
		if err := p.recordAudit(ctx, call, session.PendingEvent{
			Type:              session.EventApprovalResolved,
			CorrelationID:     call.CallID,
			RuntimeSnapshotID: call.RuntimeSnapshotID,
			Payload:           session.MustJSONPayload(map[string]any{"actor": call.Actor, "turn_id": call.TurnID, "call_id": call.CallID, "approved": approved}),
		}); err != nil {
			return fmt.Errorf("record approval result: %w", err)
		}
		emitLiveEvent(ctx, LiveEvent{
			Type: LiveApprovalResolved, CallID: call.CallID, TurnID: call.TurnID,
			ToolName: request.ToolName, Approved: approved,
		})
		if !approved {
			return errors.Join(ErrApprovalDenied, p.recordFailure(ctx, call, binding, ErrApprovalDenied))
		}
		return nil
	default:
		return fmt.Errorf("unsupported policy effect %q", decision.Effect)
	}
}

// Once a decision exists it must remain auditable even if the caller cancels.
// This does not grant execution after cancellation: policy/approver/executor
// still receive the original context and tool.started remains cancelable.
func (p *Pipeline) recordAudit(ctx context.Context, call Call, event session.PendingEvent) error {
	durable, cancel := p.durableContext(ctx)
	defer cancel()
	_, err := p.events.Append(durable, call.SessionID, session.AnySequence, event)
	return err
}

func (p *Pipeline) recordStart(ctx context.Context, call Call, binding Binding) error {
	_, err := p.events.Append(ctx, call.SessionID, session.AnySequence, session.PendingEvent{
		Type:              session.EventToolStarted,
		CorrelationID:     call.CallID,
		RuntimeSnapshotID: call.RuntimeSnapshotID,
		Payload: session.MustJSONPayload(map[string]any{
			"actor":     call.Actor,
			"execution": call.Execution,
			"turn_id":   call.TurnID,
			"call_id":   call.CallID,
			"tool":      binding.Descriptor.Name,
			"revision":  binding.Descriptor.Revision,
		}),
	})
	if err != nil {
		return fmt.Errorf("record tool start: %w", err)
	}
	emitLiveEvent(ctx, LiveEvent{
		Type: LiveToolStarted, CallID: call.CallID, TurnID: call.TurnID,
		ToolName: binding.Descriptor.Name,
	})
	return nil
}

func (p *Pipeline) invoke(ctx context.Context, call Call, binding Binding, snapshot *Snapshot) (json.RawMessage, error) {
	ctx = orchestration.WithActorScope(ctx, call.Actor)
	ctx = orchestration.WithToolCallID(ctx, call.CallID)
	ctx = orchestration.WithTurnID(ctx, call.TurnID)
	ctx = withPipeline(ctx, p)
	ctx = withInvocation(ctx, Invocation{
		SessionID: call.SessionID, TurnID: call.TurnID, Execution: call.Execution,
		RuntimeSnapshotID: call.RuntimeSnapshotID, CallID: call.CallID,
		RegistryGeneration: snapshot.Generation(),
	})
	data, err := binding.Executor.Invoke(ctx, append(json.RawMessage(nil), call.Arguments...))
	if err != nil {
		if suspended, ok := errors.AsType[*orchestration.WaitSuspendedError](err); ok {
			// Flow operators may use the domain suspension signal without
			// owning a Supervisor wait set. Preserve that control signal; the
			// Agent tool path supplies the full durable identity below.
			if suspended.WaitSet.TeamID == "" {
				return nil, suspended
			}
			// A suspension is control flow, not a failed tool result. Persist
			// the intent before Eino checkpoints; Supervisor makes it runnable
			// only after that checkpoint has been durably saved.
			if recordErr := p.recordWaitSuspension(ctx, call, binding, suspended); recordErr != nil {
				return nil, recordErr
			}
			return nil, suspended
		}
		executionErr := &ExecutionError{Tool: binding.Descriptor.Name + "@" + binding.Descriptor.Revision, Err: err}
		failure := error(executionErr)
		var fatal interface{ ToolExecutionFatal() bool }
		if errors.As(err, &fatal) && fatal.ToolExecutionFatal() {
			failure = err
		}
		if recordErr := p.recordFailure(ctx, call, binding, failure); recordErr != nil {
			// Do not wrap ExecutionError here: adapters classify it as a
			// recoverable tool result, which would hide a durability failure.
			return nil, fmt.Errorf("tool failed (%v) and its failure could not be persisted: %w", err, recordErr)
		}
		if errors.As(err, &fatal) && fatal.ToolExecutionFatal() {
			return nil, err
		}
		return nil, executionErr
	}
	if len(data) == 0 || !json.Valid(data) {
		invalid := errors.New("tool returned invalid JSON")
		return nil, errors.Join(invalid, p.recordFailure(ctx, call, binding, invalid))
	}
	return append(json.RawMessage(nil), data...), nil
}

func (p *Pipeline) recordSuccess(ctx context.Context, call Call, binding Binding, data json.RawMessage) error {
	// Successful side effects must remain recordable after user cancellation.
	durable, cancel := p.durableContext(ctx)
	defer cancel()
	_, err := p.events.Append(durable, call.SessionID, session.AnySequence, session.PendingEvent{
		Type:              session.EventToolSucceeded,
		CorrelationID:     call.CallID,
		RuntimeSnapshotID: call.RuntimeSnapshotID,
		Payload: session.MustJSONPayload(map[string]any{
			"actor":     call.Actor,
			"execution": call.Execution,
			"turn_id":   call.TurnID,
			"call_id":   call.CallID,
			"tool":      binding.Descriptor.Name,
			"revision":  binding.Descriptor.Revision,
			"result":    json.RawMessage(data),
		}),
	})
	if err != nil {
		// Execution succeeded but durability is unknown. Surface an explicit error;
		// callers must not retry a possibly side-effecting tool automatically.
		emitLiveEvent(ctx, LiveEvent{
			Type: LiveToolOutcomeUnknown, CallID: call.CallID, TurnID: call.TurnID,
			ToolName: binding.Descriptor.Name, Err: err,
		})
		return &OutcomeUnknownError{CallID: call.CallID, Tool: binding.Descriptor.Name, Cause: err}
	}
	emitLiveEvent(ctx, LiveEvent{
		Type: LiveToolSucceeded, CallID: call.CallID, TurnID: call.TurnID,
		ToolName: binding.Descriptor.Name,
	})
	return nil
}

func (p *Pipeline) validate() error {
	if p.registry == nil || p.policy == nil || p.events == nil || p.calls == nil {
		return errors.New("tool pipeline requires registry, policy and event store")
	}
	return nil
}

func (p *Pipeline) replayOutcome(ctx context.Context, call Call, binding Binding) (Result, bool, error) {
	events, err := correlatedEvents(ctx, p.events, call.SessionID, call.CallID)
	if err != nil {
		return Result{}, false, fmt.Errorf("recover durable tool call: %w", err)
	}
	var proposed struct {
		Tool      string                         `json:"tool"`
		Revision  string                         `json:"revision"`
		Arguments json.RawMessage                `json:"arguments"`
		TurnID    string                         `json:"turn_id"`
		Execution orchestration.ExecutionBinding `json:"execution"`
	}
	proposalFound := false
	started := false
	for _, event := range events {
		if event.Type == session.EventToolStarted {
			var identity struct {
				TurnID    string                         `json:"turn_id"`
				Execution orchestration.ExecutionBinding `json:"execution"`
			}
			if err := json.Unmarshal(event.Payload, &identity); err != nil {
				return Result{}, false, fmt.Errorf("decode started tool call: %w", err)
			}
			started = started || identity.TurnID == call.TurnID && identity.Execution == call.Execution
		}
		if event.Type == session.EventToolProposed {
			var candidate struct {
				Tool      string                         `json:"tool"`
				Revision  string                         `json:"revision"`
				Arguments json.RawMessage                `json:"arguments"`
				TurnID    string                         `json:"turn_id"`
				Execution orchestration.ExecutionBinding `json:"execution"`
			}
			if err := json.Unmarshal(event.Payload, &candidate); err != nil {
				return Result{}, false, fmt.Errorf("decode durable tool proposal: %w", err)
			}
			if candidate.TurnID != call.TurnID {
				continue
			}
			if candidate.Execution != call.Execution {
				return Result{}, false, fmt.Errorf("%w: %s", ErrCallConflict, call.CallID)
			}
			proposed = candidate
			proposalFound = true
		}
		if event.Type == session.EventToolFailed {
			var failed struct {
				Tool      string                         `json:"tool"`
				Revision  string                         `json:"revision"`
				TurnID    string                         `json:"turn_id"`
				Execution orchestration.ExecutionBinding `json:"execution"`
				Receipt   *failureReceipt                `json:"recovery"`
			}
			if err := json.Unmarshal(event.Payload, &failed); err != nil {
				return Result{}, false, fmt.Errorf("decode durable tool failure: %w", err)
			}
			if failed.TurnID != call.TurnID || failed.Execution != call.Execution || failed.Receipt == nil {
				continue
			}
			if !proposalFound || proposed.Tool != binding.Descriptor.Name || proposed.Revision != binding.Descriptor.Revision ||
				failed.Tool != binding.Descriptor.Name || failed.Revision != binding.Descriptor.Revision || !equalJSON(proposed.Arguments, call.Arguments) {
				return Result{}, false, fmt.Errorf("%w: %s", ErrCallConflict, call.CallID)
			}
			failure, valid := failed.Receipt.restore(binding)
			if !valid {
				return Result{}, false, errors.New("durable tool failure recovery receipt is invalid")
			}
			return Result{}, true, failure
		}
		if event.Type != session.EventToolSucceeded {
			continue
		}
		var succeeded struct {
			Tool      string                         `json:"tool"`
			Revision  string                         `json:"revision"`
			Result    json.RawMessage                `json:"result"`
			TurnID    string                         `json:"turn_id"`
			Execution orchestration.ExecutionBinding `json:"execution"`
		}
		if err := json.Unmarshal(event.Payload, &succeeded); err != nil {
			return Result{}, false, fmt.Errorf("decode durable tool result: %w", err)
		}
		if succeeded.TurnID != call.TurnID || succeeded.Execution != call.Execution {
			continue
		}
		if !proposalFound || proposed.Tool != binding.Descriptor.Name || proposed.Revision != binding.Descriptor.Revision ||
			succeeded.Tool != binding.Descriptor.Name || succeeded.Revision != binding.Descriptor.Revision ||
			!equalJSON(proposed.Arguments, call.Arguments) {
			return Result{}, false, fmt.Errorf("%w: %s", ErrCallConflict, call.CallID)
		}
		if len(succeeded.Result) == 0 || !json.Valid(succeeded.Result) {
			return Result{}, false, errors.New("durable tool result is invalid JSON")
		}
		return Result{CallID: call.CallID, Revision: succeeded.Revision, Data: append(json.RawMessage(nil), succeeded.Result...)}, true, nil
	}
	if proposalFound && (proposed.Tool != binding.Descriptor.Name || proposed.Revision != binding.Descriptor.Revision || !equalJSON(proposed.Arguments, call.Arguments)) {
		return Result{}, false, fmt.Errorf("%w: %s", ErrCallConflict, call.CallID)
	}
	if started && mayHaveSideEffects(binding.Descriptor.RiskClass) {
		return Result{}, false, &OutcomeUnknownError{CallID: call.CallID, Tool: binding.Descriptor.Name}
	}
	return Result{}, false, nil
}

func correlatedEvents(ctx context.Context, store session.EventStore, sessionID session.ID, correlationID string) ([]session.Event, error) {
	if indexed, ok := store.(session.CorrelatedEventStore); ok {
		return indexed.LoadByCorrelation(ctx, sessionID, correlationID)
	}
	var matched []session.Event
	err := session.VisitEvents(ctx, store, sessionID, func(event session.Event) error {
		if event.CorrelationID == correlationID {
			matched = append(matched, event)
		}
		return nil
	})
	return matched, err
}

func equalJSON(left, right json.RawMessage) bool {
	var compactLeft, compactRight bytes.Buffer
	if json.Compact(&compactLeft, left) != nil || json.Compact(&compactRight, right) != nil {
		return false
	}
	return bytes.Equal(compactLeft.Bytes(), compactRight.Bytes())
}

type callCoordinator struct {
	mu    sync.Mutex
	locks map[string]*callLock
}

type callLock struct {
	mu   sync.Mutex
	refs int
}

func newCallCoordinator() *callCoordinator {
	return &callCoordinator{locks: make(map[string]*callLock)}
}

func (coordinator *callCoordinator) lock(key string) func() {
	coordinator.mu.Lock()
	entry := coordinator.locks[key]
	if entry == nil {
		entry = &callLock{}
		coordinator.locks[key] = entry
	}
	entry.refs++
	coordinator.mu.Unlock()
	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		coordinator.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(coordinator.locks, key)
		}
		coordinator.mu.Unlock()
	}
}

func (p *Pipeline) recordFailure(ctx context.Context, call Call, binding Binding, cause error) error {
	recordContext, cancel := p.durableContext(ctx)
	defer cancel()
	payload := map[string]any{
		"actor": call.Actor, "execution": call.Execution, "turn_id": call.TurnID, "call_id": call.CallID,
		"tool": binding.Descriptor.Name, "revision": binding.Descriptor.Revision, "error": cause.Error(),
	}
	if precondition, ok := errors.AsType[*PreconditionError](cause); ok {
		payload["error_code"] = precondition.Code
	}
	if receipt := recoverableFailure(cause); receipt != nil {
		payload["recovery"] = receipt
	}
	_, err := p.events.Append(recordContext, call.SessionID, session.AnySequence, session.PendingEvent{
		Type:              session.EventToolFailed,
		CorrelationID:     call.CallID,
		RuntimeSnapshotID: call.RuntimeSnapshotID,
		Payload:           session.MustJSONPayload(payload),
	})
	if err != nil {
		return fmt.Errorf("record tool failure: %w", err)
	}
	emitLiveEvent(ctx, LiveEvent{
		Type: LiveToolFailed, CallID: call.CallID, TurnID: call.TurnID,
		ToolName: binding.Descriptor.Name, Err: cause,
	})
	return nil
}

// durableContext preserves trusted invocation values after cancellation while
// retaining the host's finite storage budget.
func (p *Pipeline) durableContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), p.durabilityTimeout)
}

func callID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate tool call ID: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}
