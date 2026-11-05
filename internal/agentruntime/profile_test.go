package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentprofile"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/policy"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/memorystore"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenizer"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

func runtimeTestProfile() agentprofile.Profile {
	return agentprofile.Profile{Name: "worker", Revision: "worker-v1", ModelProfile: "primary", Instruction: "Only follow the child instruction.", ContextInheritance: agentprofile.InheritSummary, WorkspaceMode: agentprofile.WorkspaceSharedLease, Limits: agentprofile.Limits{MaxIterations: 3, MaxToolCalls: 4, MaxWallTime: time.Minute}}
}

func runtimeTestScope(t *testing.T) orchestration.ActorScope {
	t.Helper()
	team, _ := orchestration.NewTeamID()
	agent, _ := orchestration.NewAgentID()
	thread, _ := orchestration.NewThreadID()
	return orchestration.ActorScope{TeamID: team, AgentID: agent, ThreadID: thread, ProfileRevision: "worker-v1"}
}

func profileFactory(t *testing.T, store *memorystore.Store, fake model.BaseChatModel, bindings ...tool.Binding) *Factory {
	t.Helper()
	registry := tool.NewRegistry()
	if len(bindings) > 0 {
		if _, err := registry.ActivateMany(bindings...); err != nil {
			t.Fatal(err)
		}
	}
	engine, err := policy.DefaultDeny(policy.Rule{ID: "test", ToolPattern: "*", CapabilityPattern: "*", Effect: policy.EffectAllow})
	if err != nil {
		t.Fatal(err)
	}
	pipeline, err := tool.NewPipeline(tool.PipelineConfig{Registry: registry, Policy: engine, Events: store})
	if err != nil {
		t.Fatal(err)
	}
	factory, err := NewFactory(Config{Instruction: "parent instruction", ModelProfile: "primary", Provider: "test", Model: "parent", MaxOutputTokens: 100, Tokenizer: fixedContextCounter{}}, fake, registry, pipeline, store, store)
	if err != nil {
		t.Fatal(err)
	}
	return factory
}

func profileBinding(name string, risk tool.RiskClass, execute tool.InvokeFunc) tool.Binding {
	return tool.Binding{Descriptor: tool.Descriptor{Name: name, Description: name, Revision: "v1", InputSchema: json.RawMessage(`{"type":"object"}`), RiskClass: risk}, Executor: execute}
}

type inspectingProfileModel struct {
	input       []*schema.Message
	tools       []string
	outputLimit int
}

func (fake *inspectingProfileModel) Generate(_ context.Context, input []*schema.Message, options ...model.Option) (*schema.Message, error) {
	fake.input = input
	common := model.GetCommonOptions(nil, options...)
	fake.tools = nil
	for _, candidate := range common.Tools {
		fake.tools = append(fake.tools, candidate.Name)
	}
	if common.MaxTokens != nil {
		fake.outputLimit = *common.MaxTokens
	}
	return schema.AssistantMessage("done", nil), nil
}

func (fake *inspectingProfileModel) Stream(ctx context.Context, input []*schema.Message, options ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	message, err := fake.Generate(ctx, input, options...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{message}), nil
}

func TestProfileAppliedToEinoToolsInstructionModelAndSession(t *testing.T) {
	ctx := context.Background()
	store := memorystore.New()
	parentModel, childModel := &inspectingProfileModel{}, &inspectingProfileModel{}
	noop := tool.InvokeFunc(func(context.Context, json.RawMessage) (json.RawMessage, error) { return json.RawMessage(`{}`), nil })
	parent := profileFactory(t, store, parentModel, profileBinding("read", tool.RiskRead, noop), profileBinding("write", tool.RiskWrite, noop))
	profile, scope := runtimeTestProfile(), runtimeTestScope(t)
	profile.Tools, profile.ModelProfile, profile.WorkspaceMode = []string{"read", "write"}, "secondary", agentprofile.WorkspaceSharedReadonly
	profile.Limits.MaxOutputTokens = 17
	if _, err := parent.WithProfile(ctx, scope, "child", profile); err == nil {
		t.Fatal("unknown model silently inherited parent")
	}
	parent.config.ResolveModel = func(_ context.Context, alias string) (ModelBinding, error) {
		if alias != "secondary" {
			t.Fatalf("alias=%s", alias)
		}
		return ModelBinding{Model: childModel, Tokenizer: fixedContextCounter{}, Provider: "test", Name: "selected", ContextWindowTokens: 2000}, nil
	}
	child, err := parent.WithProfile(ctx, scope, "child", profile)
	if err != nil {
		t.Fatal(err)
	}
	if child.config.MaxIterations != 3 || child.config.Model != "selected" || parent.config.Instruction != "parent instruction" {
		t.Fatal("profile not applied or parent mutated")
	}
	if _, err := child.NewTurn(ctx, "other-session"); err == nil {
		t.Fatal("profile factory accepted foreign session")
	}
	turn, err := child.NewTurn(ctx, "child")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := turn.Run(ctx, []*schema.Message{schema.UserMessage("hello")}, nil); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(childModel.tools, []string{"read"}) || childModel.outputLimit != 17 {
		t.Fatalf("tools=%v output=%d", childModel.tools, childModel.outputLimit)
	}
	if len(parentModel.input) != 0 || len(childModel.input) == 0 || !strings.Contains(childModel.input[0].Content, profile.Instruction) {
		t.Fatal("wrong model/instruction used")
	}
}

func TestProfileToolGuardEnforcesConcurrentCumulativeBudgetAndReplay(t *testing.T) {
	ctx := context.Background()
	store := memorystore.New()
	var executed atomic.Int32
	binding := profileBinding("read", tool.RiskRead, func(context.Context, json.RawMessage) (json.RawMessage, error) {
		executed.Add(1)
		return json.RawMessage(`{}`), nil
	})
	parent := profileFactory(t, store, &summaryModel{}, binding)
	profile, scope := runtimeTestProfile(), runtimeTestScope(t)
	profile.Tools, profile.Limits.MaxToolCalls = []string{"read"}, 2
	child, err := parent.WithProfile(ctx, scope, "child", profile)
	if err != nil {
		t.Fatal(err)
	}
	call := tool.Call{Actor: scope, SessionID: "child", TurnID: "t1", ToolName: "read", CallID: "one", Arguments: json.RawMessage(`{}`)}
	if _, err := child.pipeline.Execute(ctx, call); err != nil {
		t.Fatal(err)
	}
	if _, err := child.pipeline.Execute(ctx, call); err != nil {
		t.Fatalf("replay: %v", err)
	}
	var success atomic.Int32
	var workers sync.WaitGroup
	for i := range 8 {
		workers.Go(func() {
			attempt := call
			attempt.CallID = fmt.Sprintf("parallel-%d", i)
			_, err := child.pipeline.Execute(ctx, attempt)
			if err == nil {
				success.Add(1)
			} else if _, ok := errors.AsType[*ProfileConstraintError](err); !ok {
				t.Errorf("unexpected admission failure: %v", err)
			}
		})
	}
	workers.Wait()
	if success.Load() != 1 || executed.Load() != 2 {
		t.Fatalf("success=%d executed=%d", success.Load(), executed.Load())
	}
	restarted, err := parent.WithProfile(ctx, scope, "child", profile)
	if err != nil {
		t.Fatal(err)
	}
	call.TurnID, call.CallID = "t2", "after-restart"
	var constraint *ProfileConstraintError
	if _, err := restarted.pipeline.Execute(ctx, call); !errors.As(err, &constraint) {
		t.Fatalf("budget reset: %v", err)
	}
	call.SessionID = "foreign"
	if _, err := restarted.pipeline.Execute(ctx, call); !errors.As(err, &constraint) {
		t.Fatalf("foreign session: %v", err)
	}
}

func TestProfileResumeRejectsInstructionAndLimitDrift(t *testing.T) {
	ctx := context.Background()
	store := memorystore.New()
	parent := profileFactory(t, store, &summaryModel{})
	profile, scope := runtimeTestProfile(), runtimeTestScope(t)
	child, err := parent.WithProfile(ctx, scope, "child", profile)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Append(ctx, "child", session.AnySequence, session.PendingEvent{Type: session.EventTurnStarted, CorrelationID: "turn", Payload: session.MustJSONPayload(map[string]any{"execution_config_id": child.executionConfigID()})})
	if err != nil {
		t.Fatal(err)
	}
	if err := child.validateTurnModel(ctx, "child", "turn"); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*agentprofile.Profile){func(p *agentprofile.Profile) { p.Instruction = "different" }, func(p *agentprofile.Profile) { p.Limits.MaxToolCalls++ }} {
		changed := profile
		change(&changed)
		view, err := parent.WithProfile(ctx, scope, "child", changed)
		if err != nil {
			t.Fatal(err)
		}
		if err := view.validateTurnModel(ctx, "child", "turn"); !errors.Is(err, ErrTurnModelChanged) {
			t.Fatalf("config drift accepted: %v", err)
		}
	}
}

func TestInheritedContextUsesTokenizerAndPreservesUTF8(t *testing.T) {
	factory := &Factory{tokenizer: tokenizer.NewApproximate("test")}
	input := strings.Repeat("中文 background ", 100)
	trimmed := factory.TrimInheritedContext(input, 30)
	if trimmed == "" || len(trimmed) >= len(input) || !utf8.ValidString(trimmed) || factory.tokenizer.CountMessages([]*schema.Message{schema.UserMessage(trimmed)}) > 30 {
		t.Fatalf("invalid trimmed seed: %q", trimmed)
	}
}

func TestInheritedContextUsesRecipientModelTokenizer(t *testing.T) {
	factory := profileFactory(t, memorystore.New(), &summaryModel{})
	profile := runtimeTestProfile()
	profile.ModelProfile, profile.Limits.MaxInheritedTokens = "recipient", 30
	recipient := tokenizer.NewApproximate("recipient")
	factory.config.ResolveModel = func(context.Context, string) (ModelBinding, error) {
		return ModelBinding{Model: &summaryModel{}, Tokenizer: recipient, Provider: "test", Name: "recipient", ContextWindowTokens: 1000}, nil
	}
	input := strings.Repeat("中文 background ", 100)
	// The parent's fixed counter reports 30 tokens regardless of text size.
	trimmed, err := factory.TrimInheritedContextForProfile(context.Background(), input, profile)
	if err != nil || trimmed == "" || len(trimmed) >= len(input) || recipient.CountMessages([]*schema.Message{schema.UserMessage(trimmed)}) > 30 {
		t.Fatalf("recipient bound ignored: %q %v", trimmed, err)
	}
}

func drainModelStream(stream *schema.StreamReader[*schema.Message]) error {
	defer stream.Close()
	for {
		if _, err := stream.Recv(); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func TestChildCalibrationDoesNotChangeRootContextEstimate(t *testing.T) {
	root := profileFactory(t, memorystore.New(), &assessmentModel{})
	root.tokenizer = tokenizer.NewApproximate("shared")
	root.config.Tokenizer = root.tokenizer
	messages := []*schema.Message{schema.UserMessage(strings.Repeat("context ", 50))}
	before := root.ContextUsage(messages)
	child, err := root.WithActorScope(runtimeTestScope(t))
	if err != nil {
		t.Fatal(err)
	}
	child.observeProviderInput("child-request", before.Used+5000, before.Used)
	if after := root.ContextUsage(messages); after.Used != before.Used || after.ProviderInput != before.ProviderInput || after.Status != before.Status {
		t.Fatalf("child usage changed the root estimate: before=%+v after=%+v", before, after)
	}
	if calibrated := child.ContextUsage(messages); calibrated.Used != before.Used+5000 || calibrated.ProviderInput != before.Used+5000 {
		t.Fatalf("child did not keep its own calibration: %+v", calibrated)
	}
	// Calibration the root learned before a fork is the child's starting point.
	root.observeProviderInput("root-request", before.Used+100, before.Used)
	forked, err := root.WithActorScope(runtimeTestScope(t))
	if err != nil {
		t.Fatal(err)
	}
	if inherited, current := forked.ContextUsage(messages), root.ContextUsage(messages); inherited.Used != current.Used {
		t.Fatalf("forked estimate = %d, want the root's %d", inherited.Used, current.Used)
	}
}
