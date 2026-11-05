package agentflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"sync"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

type Stage interface {
	Name() string
	Apply(context.Context, *Execution, Stream) Stream
}

type Processor interface {
	Name() string
	Process(context.Context, *Execution, string, Value) (Value, error)
}

type ProcessorFunc struct {
	ProcessorName string
	// Identity describes the operator wiring, not callback implementation.
	// Callback semantic changes must also bump the enclosing Flow revision.
	Identity string
	Function func(context.Context, *Execution, string, Value) (Value, error)
}

func (processor ProcessorFunc) Name() string { return processor.ProcessorName }

func (processor ProcessorFunc) Process(ctx context.Context, execution *Execution, stepID string, input Value) (Value, error) {
	if processor.Function == nil {
		return Value{}, errors.New("nil Flow processor")
	}
	return processor.Function(ctx, execution, stepID, input.Clone())
}

type Definition struct {
	Name     string
	Revision string
	Stages   []Stage
}

// Pipeline streams each upstream result into the next stage immediately.
func Pipeline(name, revision string, stages ...Stage) *Definition {
	return &Definition{Name: name, Revision: revision, Stages: append([]Stage(nil), stages...)}
}

func (definition *Definition) Validate() error {
	if definition == nil {
		return errors.New("Flow definition is nil")
	}
	if err := validName("Flow", definition.Name); err != nil {
		return err
	}
	if err := validRevision(definition.Revision); err != nil {
		return err
	}
	if len(definition.Stages) == 0 {
		return errors.New("Flow requires at least one stage")
	}
	seen := make(map[string]struct{}, len(definition.Stages))
	for _, stage := range definition.Stages {
		if nilInterface(stage) {
			return errors.New("Flow contains a nil stage")
		}
		if err := validName("Flow stage", stage.Name()); err != nil {
			return err
		}
		if _, exists := seen[stage.Name()]; exists {
			return fmt.Errorf("duplicate Flow stage %q", stage.Name())
		}
		seen[stage.Name()] = struct{}{}
		switch typed := stage.(type) {
		case *eachStage:
			if err := validateProcessor(typed.processor); err != nil {
				return fmt.Errorf("Flow stage %q: %w", typed.name, err)
			}
		case *parallelStage:
			if len(typed.branches) == 0 {
				return fmt.Errorf("Flow stage %q requires at least one branch", typed.name)
			}
			branches := make(map[string]struct{}, len(typed.branches))
			for _, branch := range typed.branches {
				if err := validateProcessor(branch); err != nil {
					return fmt.Errorf("Flow stage %q: %w", typed.name, err)
				}
				if _, exists := branches[branch.Name()]; exists {
					return fmt.Errorf("Flow stage %q has duplicate branch %q", typed.name, branch.Name())
				}
				branches[branch.Name()] = struct{}{}
			}
		}
	}
	return nil
}

func validateProcessor(processor Processor) error {
	if nilInterface(processor) {
		return errors.New("nil Flow processor")
	}
	if err := validName("Flow processor", processor.Name()); err != nil {
		return err
	}
	if function, ok := processor.(ProcessorFunc); ok && function.Function == nil {
		return fmt.Errorf("Flow processor %q has a nil function", function.Name())
	}
	if function, ok := processor.(*ProcessorFunc); ok && function.Function == nil {
		return fmt.Errorf("Flow processor %q has a nil function", function.Name())
	}
	return nil
}

func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

// Fingerprint identifies the declared operator topology. It complements the
// mandatory revision: topology changes are detected automatically, while host
// callback semantic changes require an explicit revision bump.
func (definition *Definition) Fingerprint() (string, error) {
	if err := definition.Validate(); err != nil {
		return "", err
	}
	type topologyStage struct {
		Kind        string   `json:"kind"`
		Name        string   `json:"name"`
		Concurrency int      `json:"concurrency,omitempty"`
		Processor   string   `json:"processor,omitempty"`
		Branches    []string `json:"branches,omitempty"`
		Predicate   bool     `json:"predicate,omitempty"`
	}
	topology := struct {
		Version  int             `json:"version"`
		Name     string          `json:"name"`
		Revision string          `json:"revision"`
		Stages   []topologyStage `json:"stages"`
	}{Version: 1, Name: definition.Name, Revision: definition.Revision, Stages: make([]topologyStage, 0, len(definition.Stages))}
	for _, stage := range definition.Stages {
		signature := topologyStage{Name: stage.Name(), Kind: reflect.TypeOf(stage).String()}
		switch typed := stage.(type) {
		case *eachStage:
			signature.Kind = "each"
			signature.Concurrency = typed.concurrency
			signature.Processor = processorIdentity(typed.processor)
		case *parallelStage:
			signature.Kind = "parallel"
			signature.Concurrency = typed.concurrency
			signature.Branches = make([]string, 0, len(typed.branches))
			for _, branch := range typed.branches {
				signature.Branches = append(signature.Branches, processorIdentity(branch))
			}
		case *compactStage:
			signature.Kind = "compact"
			signature.Predicate = typed.predicate != nil
		case *budgetStage:
			signature.Kind = "budget"
		case *returnStage:
			signature.Kind = "return"
		}
		topology.Stages = append(topology.Stages, signature)
	}
	encoded, err := json.Marshal(topology)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func processorIdentity(processor Processor) string {
	if processor == nil {
		return "<nil>"
	}
	if function, ok := processor.(ProcessorFunc); ok && function.Identity != "" {
		return function.Identity
	}
	if function, ok := processor.(*ProcessorFunc); ok && function.Identity != "" {
		return function.Identity
	}
	return reflect.TypeOf(processor).String() + ":" + processor.Name()
}

type eachStage struct {
	name        string
	processor   Processor
	concurrency int
}

// Each applies a processor concurrently to arriving items. A non-positive
// concurrency is normalized to one to keep downstream pressure bounded.
func Each(name string, concurrency int, processor Processor) Stage {
	if concurrency <= 0 {
		concurrency = 1
	}
	return &eachStage{name: name, processor: processor, concurrency: concurrency}
}

func (stage *eachStage) Name() string { return stage.name }

func (stage *eachStage) Apply(ctx context.Context, execution *Execution, input Stream) Stream {
	output := make(chan Item)
	go func() {
		defer close(output)
		if stage.processor == nil {
			sendItem(ctx, output, Item{Err: errors.New("Each stage has no processor")})
			return
		}
		semaphore := make(chan struct{}, stage.concurrency)
		var workers sync.WaitGroup
		seen := make(map[string]int)
		ordinal := 0
		for item := range input {
			if item.Err != nil || item.Final {
				if !sendItem(ctx, output, item) {
					break
				}
				continue
			}
			identity := itemIdentity(item, stage.name, ordinal, seen)
			ordinal++
			item.identity = identity
			item.Value.identity = identity
			select {
			case semaphore <- struct{}{}:
			case <-ctx.Done():
				workers.Wait()
				return
			}
			workers.Add(1)
			go func(item Item) {
				defer workers.Done()
				defer func() { <-semaphore }()
				stepID := stageStepID(stage.name, stage.processor.Name(), item.identity)
				values, err := execution.Do(ctx, stepID, item.Value, func(actionContext context.Context) ([]Value, error) {
					value, processErr := stage.processor.Process(actionContext, execution, stepID, item.Value)
					if processErr != nil {
						return []Value{value}, processErr
					}
					return []Value{value}, nil
				})
				result := Item{Err: err, identity: item.identity}
				if len(values) > 0 {
					result.Value = values[0]
					result.Value.identity = item.identity
				}
				sendItem(ctx, output, result)
			}(item)
		}
		workers.Wait()
	}()
	return output
}

type parallelStage struct {
	name        string
	branches    []Processor
	concurrency int
}

// Parallel dispatches all branches concurrently and emits each branch result
// as soon as it completes. Downstream stages need not wait for a join barrier.
// A bounded worker budget prevents an untrusted or very large upstream stream
// from creating an unbounded number of goroutines. Use ParallelWithConcurrency
// when a Flow needs a different limit.
func Parallel(name string, branches ...Processor) Stage {
	return ParallelWithConcurrency(name, 16, branches...)
}

// ParallelWithConcurrency is Parallel with an explicit maximum number of
// input items being processed at once. Every admitted item still fans out to
// all branches concurrently, preserving the streaming branch semantics.
func ParallelWithConcurrency(name string, concurrency int, branches ...Processor) Stage {
	if concurrency <= 0 {
		concurrency = 1
	}
	return &parallelStage{name: name, branches: append([]Processor(nil), branches...), concurrency: concurrency}
}

func (stage *parallelStage) Name() string { return stage.name }

func (stage *parallelStage) Apply(ctx context.Context, execution *Execution, input Stream) Stream {
	output := make(chan Item)
	go func() {
		defer close(output)
		if len(stage.branches) == 0 {
			sendItem(ctx, output, Item{Err: errors.New("parallel stage requires at least one branch")})
			return
		}
		var workers sync.WaitGroup
		capacity := max(stage.concurrency*len(stage.branches), 1)
		semaphore := make(chan struct{}, capacity)
		seen := make(map[string]int)
		ordinal := 0
		for item := range input {
			if item.Err != nil || item.Final {
				sendItem(ctx, output, item)
				continue
			}
			identity := itemIdentity(item, stage.name, ordinal, seen)
			ordinal++
			item.identity = identity
			for _, branch := range stage.branches {
				if branch == nil {
					sendItem(ctx, output, Item{Err: errors.New("parallel stage contains a nil branch")})
					continue
				}
				select {
				case semaphore <- struct{}{}:
				case <-ctx.Done():
					workers.Wait()
					return
				}
				workers.Add(1)
				go func(branch Processor, item Item, parentIdentity string) {
					defer workers.Done()
					defer func() { <-semaphore }()
					branchIdentity := parentIdentity + "/" + branch.Name()
					branchInput := item.Value.Clone()
					branchInput.identity = branchIdentity
					if branchInput.Key == "" {
						branchInput.Key = branch.Name()
					} else {
						branchInput.Key += "." + branch.Name()
					}
					stepID := stageStepID(stage.name, branch.Name(), branchIdentity)
					values, err := execution.Do(ctx, stepID, branchInput, func(actionContext context.Context) ([]Value, error) {
						value, processErr := branch.Process(actionContext, execution, stepID, branchInput)
						return []Value{value}, processErr
					})
					result := Item{Err: err, identity: branchIdentity}
					if len(values) > 0 {
						result.Value = values[0]
						result.Value.identity = branchIdentity
					}
					sendItem(ctx, output, result)
				}(branch, item, identity)
			}
		}
		workers.Wait()
	}()
	return output
}

type compactStage struct {
	name      string
	predicate func(Value) bool
}

// Compact filters failed, null, empty, or caller-invalid branch results.
func Compact(name string, predicate func(Value) bool) Stage {
	return &compactStage{name: name, predicate: predicate}
}

func (stage *compactStage) Name() string { return stage.name }

func (stage *compactStage) Apply(ctx context.Context, _ *Execution, input Stream) Stream {
	output := make(chan Item)
	go func() {
		defer close(output)
		for item := range input {
			if item.Err != nil || item.Value.Empty() {
				continue
			}
			if stage.predicate != nil && !stage.predicate(item.Value.Clone()) {
				continue
			}
			if !sendItem(ctx, output, item) {
				return
			}
		}
	}()
	return output
}

type budgetStage struct{ name string }

// Budget exposes the current durable team budget as the "budget" variable.
func Budget(name string) Stage { return &budgetStage{name: name} }

func (stage *budgetStage) Name() string { return stage.name }

func (stage *budgetStage) Apply(ctx context.Context, execution *Execution, input Stream) Stream {
	return Each(stage.name, 1, ProcessorFunc{ProcessorName: "read", Identity: "budget", Function: func(ctx context.Context, execution *Execution, stepID string, value Value) (Value, error) {
		result, err := execution.Tool(ctx, stepID+".call", BudgetToolName, map[string]any{})
		if err != nil {
			return Value{}, err
		}
		value = value.Clone()
		if value.Vars == nil {
			value.Vars = make(map[string]json.RawMessage)
		}
		value.Vars["budget"] = result
		return value, nil
	}}).Apply(ctx, execution, input)
}

type returnStage struct{ name string }

// Return terminates the Flow when the first valid item reaches this stage.
func Return(name string) Stage { return &returnStage{name: name} }

func (stage *returnStage) Name() string { return stage.name }

func (stage *returnStage) Apply(ctx context.Context, _ *Execution, input Stream) Stream {
	output := make(chan Item)
	go func() {
		defer close(output)
		item, ok := <-input
		if !ok {
			return
		}
		if item.Err == nil {
			item.Final = true
		}
		sendItem(ctx, output, item)
	}()
	return output
}

// Map creates a pure processor. External actions belong in Tool or
// AgentSession so they cannot bypass policy and event recording.
func Map(name string, function func(context.Context, Value) (Value, error)) Processor {
	return ProcessorFunc{ProcessorName: name, Identity: "map:" + name, Function: func(ctx context.Context, _ *Execution, _ string, value Value) (Value, error) {
		if function == nil {
			return Value{}, errors.New("nil map function")
		}
		return function(ctx, value)
	}}
}

type ToolArguments func(Value) (any, error)

func Tool(name, toolName string, arguments ToolArguments) Processor {
	return ProcessorFunc{ProcessorName: name, Identity: "tool:" + name + ":" + toolName, Function: func(ctx context.Context, execution *Execution, stepID string, input Value) (Value, error) {
		if arguments == nil {
			return Value{}, errors.New("tool processor requires an argument builder")
		}
		built, err := arguments(input.Clone())
		if err != nil {
			return Value{}, err
		}
		data, err := execution.Tool(ctx, stepID+".call", toolName, built)
		if err != nil {
			return Value{}, err
		}
		input.Data = data
		return input, nil
	}}
}

type PromptBuilder func(Value) (string, error)

type AgentSessionConfig struct {
	Name          string
	Profile       string
	ContextPolicy string
	Prompt        PromptBuilder
	WaitTimeout   time.Duration
}

// AgentSession reuses one durable child thread for repeated stages. Spawn,
// continuation and synchronization are ordinary policy-controlled tools.
func AgentSession(config AgentSessionConfig) Processor {
	identity := fmt.Sprintf("agent_session:%s:%s:%s:%d", config.Name, config.Profile, config.ContextPolicy, config.WaitTimeout)
	return ProcessorFunc{ProcessorName: config.Name, Identity: identity, Function: func(ctx context.Context, execution *Execution, stepID string, input Value) (Value, error) {
		if config.Prompt == nil {
			return Value{}, errors.New("agent_session requires a prompt builder")
		}
		prompt, err := config.Prompt(input.Clone())
		if err != nil {
			return Value{}, err
		}
		if prompt == "" {
			return Value{}, errors.New("agent_session prompt is empty")
		}
		sessionKey := input.Key
		if input.identity != "" {
			sessionKey = input.identity + "\x00" + sessionKey
		}
		sessionName := execution.sessionName(config.Name, sessionKey)
		sessionStage, err := execution.runtime.store.BeginSessionStage(ctx, execution.run.ID, sessionName, stepID)
		if err != nil {
			return Value{}, err
		}
		var agentID orchestration.AgentID
		switch sessionStage.Action {
		case SessionSpawn:
			spawned, err := execution.Tool(ctx, stepID+".spawn", "agent_spawn", map[string]any{
				"prompt": prompt, "profile": config.Profile, "context_policy": config.ContextPolicy,
				"idempotency_key": execution.run.ID + ":" + stepID + ":spawn",
			})
			if err != nil {
				return Value{}, err
			}
			var response struct {
				AgentID orchestration.AgentID `json:"agent_id"`
			}
			if err := json.Unmarshal(spawned, &response); err != nil || response.AgentID == "" {
				return Value{}, fmt.Errorf("decode agent_spawn result: %w", err)
			}
			agentID = response.AgentID
			if err := execution.runtime.store.BindSessionAgent(ctx, execution.run.ID, sessionName, agentID); err != nil {
				return Value{}, err
			}
		case SessionContinue:
			var exists bool
			agentID, exists, err = execution.runtime.store.SessionAgent(ctx, execution.run.ID, sessionName)
			if err != nil {
				return Value{}, err
			}
			if !exists {
				return Value{}, fmt.Errorf("agent_session %s continuation has no bound Agent", config.Name)
			}
			if _, err := execution.Tool(ctx, stepID+".continue", "agent_continue", map[string]any{
				"agent_id": agentID, "prompt": prompt,
				"idempotency_key": execution.run.ID + ":" + stepID + ":continue",
			}); err != nil {
				return Value{}, err
			}
		default:
			return Value{}, fmt.Errorf("agent_session %s has invalid durable action %q", config.Name, sessionStage.Action)
		}
		timeout := int(config.WaitTimeout / time.Millisecond)
		if timeout <= 0 {
			timeout = int((30 * time.Minute) / time.Millisecond)
		}
		waited, err := execution.Tool(ctx, stepID+".wait", "agent_wait", map[string]any{
			"agent_ids": []orchestration.AgentID{agentID}, "timeout_ms": timeout,
		})
		if err != nil {
			return Value{}, err
		}
		var wait struct {
			Agents    []orchestration.AgentSnapshot `json:"agents"`
			Satisfied bool                          `json:"satisfied"`
			TimedOut  bool                          `json:"timed_out"`
		}
		if err := json.Unmarshal(waited, &wait); err != nil {
			return Value{}, fmt.Errorf("decode agent_wait result: %w", err)
		}
		if wait.TimedOut || !wait.Satisfied || len(wait.Agents) != 1 {
			return Value{}, fmt.Errorf("agent_session %s did not complete before its wait deadline", config.Name)
		}
		agent := wait.Agents[0]
		if agent.Result == nil {
			return Value{}, fmt.Errorf("agent_session %s has no durable result", config.Name)
		}
		data, err := json.Marshal(agent.Result)
		if err != nil {
			return Value{}, err
		}
		output := input.Clone()
		output.Data = data
		if output.Vars == nil {
			output.Vars = make(map[string]json.RawMessage)
		}
		encodedAgentID, _ := json.Marshal(agentID)
		output.Vars["agent_id"] = encodedAgentID
		switch agent.Result.Status {
		case orchestration.ResultSucceeded, orchestration.ResultPartial:
			return output, nil
		default:
			return output, fmt.Errorf("agent_session %s returned %s: %s", config.Name, agent.Result.Status, agent.Result.Error)
		}
	}}
}

// Human pauses at the Tool Pipeline approval boundary. Interactive callers
// can approve; fail-closed/headless callers receive ErrHumanDeclined.
func Human(name string, question PromptBuilder) Processor {
	return ProcessorFunc{ProcessorName: name, Identity: "human:" + name, Function: func(ctx context.Context, execution *Execution, stepID string, input Value) (Value, error) {
		if question == nil {
			return Value{}, errors.New("human operator requires a question builder")
		}
		prompt, err := question(input.Clone())
		if err != nil {
			return Value{}, err
		}
		result, err := execution.Tool(ctx, stepID+".call", HumanToolName, map[string]any{"question": prompt})
		if err != nil {
			return Value{}, errors.Join(ErrHumanDeclined, err)
		}
		input = input.Clone()
		if input.Vars == nil {
			input.Vars = make(map[string]json.RawMessage)
		}
		input.Vars["human"] = result
		return input, nil
	}}
}

func stageStepID(stageName, processorName, identity string) string {
	encoded := []byte(identity)
	digest := sha256.Sum256(encoded)
	return stageName + "." + processorName + ".v2." + hex.EncodeToString(digest[:8])
}

func itemIdentity(item Item, stageName string, ordinal int, seen map[string]int) string {
	identity := item.identity
	if identity == "" {
		identity = stageName + "/" + strconv.Itoa(ordinal)
	}
	count := seen[identity]
	seen[identity] = count + 1
	if count > 0 {
		identity += "#" + strconv.Itoa(count)
	}
	return identity
}

func sendItem(ctx context.Context, output chan<- Item, item Item) bool {
	select {
	case output <- item:
		return true
	case <-ctx.Done():
		return false
	}
}
