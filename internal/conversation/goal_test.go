package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentgoal"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/policy"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/sqlitestore"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenusage"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

func TestGoalOuterLoopContinuesThenCompletesWithoutLeakingRejectedCandidate(t *testing.T) {
	t.Parallel()
	model := &goalLoopModel{responses: []*schema.Message{
		goalReportCallWithContent("draft emitted alongside report", "goal-report-1", agentgoal.DecisionContinue, "tests are still missing"),
		schema.AssistantMessage("draft answer", nil),
		goalReportCall("goal-report-2", agentgoal.DecisionComplete, "implementation and tests pass"),
		schema.AssistantMessage("final accepted answer", nil),
	}}
	conversation := newGoalConversation(t, model, agentgoal.Config{Enabled: true, Activation: agentgoal.ActivationAlways, Assessment: agentgoal.AssessmentSelf, MaxRounds: 3, MaxWallTime: time.Minute}, nil)
	var visible strings.Builder
	result, err := conversation.RunTurn(context.Background(), "implement the feature", func(_ context.Context, event agentruntime.Event) error {
		visible.WriteString(event.Delta)
		if event.Message != nil && len(event.Message.ToolCalls) == 0 {
			visible.WriteString(event.Message.Content)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.GoalState != agentgoal.StateCompleted || result.GoalRounds != 2 || result.Final == nil || result.Final.Content != "final accepted answer" {
		t.Fatalf("result = %#v", result)
	}
	if strings.Contains(visible.String(), "draft answer") || !strings.Contains(visible.String(), "final accepted answer") {
		t.Fatalf("visible candidate stream = %q", visible.String())
	}
	if conversation.CompletedTurns() != 1 || model.callCount() != 4 {
		t.Fatalf("completed=%d model calls=%d", conversation.CompletedTurns(), model.callCount())
	}
	_, store, _, _ := conversation.factory.GoalRuntime()
	admissions, ok := store.(agentgoal.AdmissionRepository)
	if !ok {
		t.Fatal("Goal store does not expose durable admissions")
	}
	consumed, err := admissions.List(context.Background(), agentgoal.AdmissionConsumed)
	if err != nil || len(consumed) != 1 || consumed[0].GoalID != result.GoalID || consumed[0].TurnID == "" || consumed[0].Round != 1 {
		t.Fatalf("admission was not consumed for exact turn/round: %+v err=%v", consumed, err)
	}
}

func TestGoalOuterLoopReportsHardCapSeparatelyFromSemanticDecision(t *testing.T) {
	t.Parallel()
	model := &goalLoopModel{responses: []*schema.Message{
		goalReportCall("goal-report-cap", agentgoal.DecisionContinue, "more work remains"),
		schema.AssistantMessage("best partial result", nil),
	}}
	conversation := newGoalConversation(t, model, agentgoal.Config{Enabled: true, Activation: agentgoal.ActivationAlways, Assessment: agentgoal.AssessmentSelf, MaxRounds: 1, MaxWallTime: time.Minute}, nil)
	result, err := conversation.RunTurn(context.Background(), "finish everything", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.GoalState != agentgoal.StateExhausted || result.GoalDecision != agentgoal.DecisionContinue || result.GoalStopReason != agentgoal.StopMaxRounds || result.GoalRounds != 1 {
		t.Fatalf("result = %#v", result)
	}
}

func TestGoalOuterLoopPausesWhenBudgetIsAlreadyExhausted(t *testing.T) {
	t.Parallel()
	model := &goalLoopModel{}
	conversation := newGoalConversation(t, model, agentgoal.Config{Enabled: true, Activation: agentgoal.ActivationAlways, Assessment: agentgoal.AssessmentSelf, MaxRounds: 3, MaxWallTime: time.Minute}, exhaustedBudget{})
	result, err := conversation.RunTurn(context.Background(), "cannot start", nil)
	if kind, paused := orchestration.ExecutionStopKindOf(err); !paused || kind != orchestration.ExecutionBudget {
		t.Fatalf("error = %v, want recoverable budget pause", err)
	}
	if result.GoalState != agentgoal.StateRunning || result.GoalStopReason != "" || result.GoalRounds != 1 || result.TurnID == "" || result.Final != nil {
		t.Fatalf("result = %#v", result)
	}
	stored, storeErr := conversation.goalStore().GetGoal(context.Background(), result.GoalID)
	if storeErr != nil || stored.State != agentgoal.StateRunning || stored.StopReason != "" {
		t.Fatalf("budget incorrectly ended durable Goal: %#v %v", stored, storeErr)
	}
	if model.callCount() != 0 {
		t.Fatalf("model was called %d times after exhausted preflight", model.callCount())
	}
}

func newGoalConversation(t *testing.T, chatModel model.BaseChatModel, goalMode agentgoal.Config, budget agentgoal.BudgetReader, bindings ...tool.Binding) *Conversation {
	return newGoalConversationWithEvaluator(t, chatModel, goalMode, budget, nil, bindings...)
}

func newGoalConversationWithEvaluator(t *testing.T, chatModel model.BaseChatModel, goalMode agentgoal.Config, budget agentgoal.BudgetReader, evaluator agentgoal.Evaluator, bindings ...tool.Binding) *Conversation {
	t.Helper()
	database, err := sqlitestore.Open(context.Background(), filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	root, _, err := database.Orchestration().EnsureRootThread(context.Background(), orchestration.RootThreadSeed{SessionID: session.ID("goal-loop-session")})
	if err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry()
	bindings = append(bindings, agentgoal.StartBinding(), agentgoal.Binding(database.Orchestration()))
	if _, err := registry.ActivateMany(bindings...); err != nil {
		t.Fatal(err)
	}
	var rules []policy.Rule
	for _, binding := range bindings {
		rules = append(rules, policy.Rule{ID: "allow-" + binding.Descriptor.Name, ToolPattern: binding.Descriptor.Name, CapabilityPattern: "tool.invoke", Effect: policy.EffectAllow})
	}
	engine, err := policy.DefaultDeny(rules...)
	if err != nil {
		t.Fatal(err)
	}
	pipeline, err := tool.NewPipeline(tool.PipelineConfig{Registry: registry, Policy: engine, Events: database.SessionEvents()})
	if err != nil {
		t.Fatal(err)
	}
	factory, err := agentruntime.NewFactory(agentruntime.Config{
		Instruction: "Help the user within their authorization.", MaxIterations: 12, ActorScope: root.ActorScope(), GoalMode: goalMode,
		GoalStore: database.Orchestration(), GoalBudget: budget, GoalEvaluator: evaluator,
	}, chatModel, registry, pipeline, database.SessionEvents(), database.Checkpoints())
	if err != nil {
		t.Fatal(err)
	}
	conversation, err := Open(context.Background(), factory, root.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	return conversation
}

func goalReportCall(id string, decision agentgoal.Decision, reason string) *schema.Message {
	arguments, _ := json.Marshal(map[string]any{"status": decision, "reason": reason})
	return schema.AssistantMessage("", []schema.ToolCall{{
		ID: id, Type: "function", Function: schema.FunctionCall{Name: agentgoal.ReportToolName, Arguments: string(arguments)},
	}})
}

func goalReportCallWithContent(content, id string, decision agentgoal.Decision, reason string) *schema.Message {
	message := goalReportCall(id, decision, reason)
	message.Content = content
	return message
}

type goalLoopModel struct {
	mu        sync.Mutex
	responses []*schema.Message
	calls     int
	goalTools [][2]bool // [goal_start, goal_report] exposure per model request
}

func (fake *goalLoopModel) Generate(_ context.Context, input []*schema.Message, options ...model.Option) (*schema.Message, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	common := model.GetCommonOptions(nil, options...)
	foundGoalTool := false
	foundStartTool := false
	for _, info := range common.Tools {
		if info.Name == agentgoal.ReportToolName {
			foundGoalTool = true
		}
		if info.Name == agentgoal.StartToolName {
			foundStartTool = true
		}
	}
	if foundGoalTool == foundStartTool {
		return nil, errors.New("exactly one phase-appropriate goal tool should be exposed")
	}
	fake.goalTools = append(fake.goalTools, [2]bool{foundStartTool, foundGoalTool})
	foundProtocol := false
	foundAdmission := false
	for _, message := range input {
		if message.Role == schema.System && strings.Contains(message.Content, agentgoal.ProtocolInstruction) {
			foundProtocol = true
		}
		if message.Role == schema.System && strings.Contains(message.Content, agentgoal.AdmissionInstruction) {
			foundAdmission = true
		}
	}
	if foundProtocol == foundAdmission || (foundProtocol && foundStartTool) {
		return nil, errors.New("goal protocol instruction does not match turn mode")
	}
	if fake.calls >= len(fake.responses) {
		return nil, errors.New("unexpected extra model iteration")
	}
	response := fake.responses[fake.calls]
	fake.calls++
	return response, nil
}

func (fake *goalLoopModel) Stream(ctx context.Context, input []*schema.Message, options ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	message, err := fake.Generate(ctx, input, options...)
	if err != nil {
		return nil, err
	}
	reader, writer := schema.Pipe[*schema.Message](1)
	go func() {
		defer writer.Close()
		writer.Send(message, nil)
	}()
	return reader, nil
}

func (fake *goalLoopModel) callCount() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.calls
}

type exhaustedBudget struct{}

func (exhaustedBudget) Budget(context.Context, orchestration.TeamID) (tokenusage.BudgetSnapshot, error) {
	return tokenusage.BudgetSnapshot{Exhausted: true}, nil
}

func TestGreetingBypassesHybridEvaluationAndPersistsAsDirectTurn(t *testing.T) {
	chatModel := &goalLoopModel{
		responses: []*schema.Message{schema.AssistantMessage("你好！", nil)},
	}
	conversation := newGoalConversation(t, chatModel, agentgoal.Config{Enabled: true, Assessment: agentgoal.AssessmentHybrid}, nil)
	result, err := conversation.RunTurn(context.Background(), "你好", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.GoalID != "" || result.Final.Content != "你好！" || chatModel.callCount() != 1 {
		t.Fatalf("greeting result=%+v, calls=%d", result, chatModel.callCount())
	}
	mode, store, _, _ := conversation.factory.GoalRuntime()
	if !mode.Enabled {
		t.Fatal("direct turn mutated the shared factory's mode")
	}
	if _, _, found, err := store.GoalForTurn(context.Background(), conversation.ID(), result.TurnID); err != nil || found {
		t.Fatalf("greeting created a goal: found=%v err=%v", found, err)
	}
	reopened, err := Open(context.Background(), conversation.factory, conversation.ID())
	if err != nil {
		t.Fatal(err)
	}
	if reopened.CompletedTurns() != 1 || len(reopened.History()) != 2 {
		t.Fatalf("reopened count=%d history=%d", reopened.CompletedTurns(), len(reopened.History()))
	}
}

func TestMixedDirectAndGoalTurnsSurviveReopen(t *testing.T) {
	chatModel := &goalLoopModel{
		responses: []*schema.Message{
			schema.AssistantMessage("你好！", nil),
			goalStartCall("start-mixed", semanticContract("修复错误并验证")),
			goalReportCall("report-1", agentgoal.DecisionContinue, "needs verification"),
			schema.AssistantMessage("draft", nil),
			goalReportCall("report-2", agentgoal.DecisionComplete, "verified"),
			schema.AssistantMessage("done", nil),
			schema.AssistantMessage("不客气！", nil),
		},
	}
	conversation := newGoalConversation(t, chatModel, agentgoal.Config{Enabled: true, Assessment: agentgoal.AssessmentSelf}, nil)
	for index, input := range []string{"你好", "你好，帮我修复这个错误", "谢谢"} {
		result, err := conversation.RunTurn(context.Background(), input, nil)
		if err != nil {
			t.Fatal(err)
		}
		if index == 1 && (result.GoalID == "" || result.GoalRounds != 2) {
			t.Fatalf("task lost its Outer Loop: %+v", result)
		}
		if index != 1 && result.GoalID != "" {
			t.Fatalf("conversation became a goal: %+v", result)
		}
		conversation, err = Open(context.Background(), conversation.factory, conversation.ID())
		if err != nil {
			t.Fatal(err)
		}
		if conversation.CompletedTurns() != index+1 {
			t.Fatalf("completed=%d want=%d", conversation.CompletedTurns(), index+1)
		}
	}
}

func TestAlwaysActivationKeepsExplicitGoalBehavior(t *testing.T) {
	chatModel := &goalLoopModel{responses: []*schema.Message{
		goalReportCall("report-always", agentgoal.DecisionComplete, "greeted"),
		schema.AssistantMessage("你好！", nil),
	}}
	conversation := newGoalConversation(t, chatModel, agentgoal.Config{Enabled: true, Activation: agentgoal.ActivationAlways, Assessment: agentgoal.AssessmentSelf}, nil)
	result, err := conversation.RunTurn(context.Background(), "你好", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.GoalState != agentgoal.StateCompleted || chatModel.callCount() != 2 {
		t.Fatalf("always activation result=%+v calls=%d", result, chatModel.callCount())
	}
}
