package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentgoal"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/codingtools"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

func semanticContract(objective string) agentgoal.Contract {
	return agentgoal.Contract{Objective: objective, Scope: "Only the task explicitly authorized by the user; ask before expanding scope.", Criteria: []agentgoal.Criterion{{Description: objective, Kind: "semantic"}}}
}

type pausedTaskModel struct {
	*goalLoopModel
	onPause func()
}

func (fake *pausedTaskModel) Stream(ctx context.Context, input []*schema.Message, options ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	if fake.callCount() == len(fake.responses) {
		if fake.onPause != nil {
			fake.onPause()
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return fake.goalLoopModel.Stream(ctx, input, options...)
}

func TestGoalDeadlineInterruptsInnerLoopAndPersistsExhaustion(t *testing.T) {
	chatModel := &pausedTaskModel{goalLoopModel: &goalLoopModel{responses: []*schema.Message{goalStartCall("start", semanticContract("Finish task"))}}}
	conversation := newGoalConversation(t, chatModel, agentgoal.Config{Enabled: true, MaxWallTime: 500 * time.Millisecond}, nil)
	started := time.Now()
	var stopped string
	result, err := conversation.RunTurn(context.Background(), "完成这个任务", func(_ context.Context, event agentruntime.Event) error {
		if event.GoalPhase != nil && event.GoalPhase.Stage == "stopped" {
			stopped = event.GoalPhase.State
		}
		return nil
	})
	if err != nil || result.GoalState != agentgoal.StateExhausted || result.GoalStopReason != agentgoal.StopMaxWallTime || result.GoalRounds != 1 || stopped != "exhausted" {
		t.Fatalf("result=%+v stopped=%s err=%v", result, stopped, err)
	}
	if time.Since(started) > 3*time.Second {
		t.Fatal("wall deadline did not interrupt execution")
	}
	_, store, _, _ := conversation.factory.GoalRuntime()
	goal, err := store.GetGoal(context.Background(), result.GoalID)
	if err != nil || goal.State != agentgoal.StateExhausted {
		t.Fatalf("persisted=%+v err=%v", goal, err)
	}
}

func TestUserCancellationIsNotResourceExhaustion(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	chatModel := &pausedTaskModel{goalLoopModel: &goalLoopModel{responses: []*schema.Message{goalStartCall("start", semanticContract("Finish task"))}}, onPause: cancel}
	conversation := newGoalConversation(t, chatModel, agentgoal.Config{Enabled: true}, nil)
	result, err := conversation.RunTurn(ctx, "完成这个任务", nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	_, store, _, _ := conversation.factory.GoalRuntime()
	goal, _, found, loadErr := store.GoalForTurn(context.Background(), conversation.ID(), result.TurnID)
	if loadErr != nil || !found || goal.State != agentgoal.StateCanceled || goal.StopReason != agentgoal.StopCanceled {
		t.Fatalf("persisted=%+v err=%v", goal, loadErr)
	}
}

func TestResumeExpiredGoalDoesNotResetDeadlineOrExecuteModel(t *testing.T) {
	chatModel := &goalLoopModel{}
	conversation := newGoalConversation(t, chatModel, agentgoal.Config{Enabled: true}, nil)
	_, store, _, _ := conversation.factory.GoalRuntime()
	contract := semanticContract("Finish original task")
	goal, _, err := store.AdmitGoal(context.Background(), agentgoal.CreateRequest{
		ID: "expired", SessionID: conversation.ID(), Text: "original task", Contract: &contract,
		Assessment: agentgoal.AssessmentSelf, MaxRounds: 3, MaxWallTime: time.Second, StartedAt: time.Now().Add(-time.Minute),
	}, "expired-turn")
	if err != nil {
		t.Fatal(err)
	}
	result, err := conversation.ResumeTurn(context.Background(), "expired-turn", nil)
	if err != nil || result.GoalID != goal.ID || result.GoalState != agentgoal.StateExhausted || chatModel.callCount() != 0 {
		t.Fatalf("result=%+v calls=%d err=%v", result, chatModel.callCount(), err)
	}
	if _, err := conversation.ResumeTurn(context.Background(), "expired-turn", nil); err == nil {
		t.Fatal("terminal goal was resumed")
	}
}

func TestFailedReadmissionDoesNotLoseOriginalTaskOrWeakenCriteria(t *testing.T) {
	_, bindings := workspaceBindings(t)
	chatModel := &goalLoopModel{responses: []*schema.Message{
		goalStartCall("original", fileContract("result.txt", "required")),
		goalStartCall("weakened", fileContract("result.txt", "wrong")),
		testToolCall("write", "workspace_write", map[string]any{"path": "result.txt", "content": "wrong"}),
		goalReportCall("report", agentgoal.DecisionComplete, "claimed done"),
		schema.AssistantMessage("candidate", nil),
	}}
	conversation := newGoalConversation(t, chatModel, agentgoal.Config{Enabled: true, MaxRounds: 1}, nil, bindings...)
	result, err := conversation.RunTurn(context.Background(), "把 result.txt 设置为 required", nil)
	if err != nil || result.GoalState != agentgoal.StateExhausted {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	_, store, _, _ := conversation.factory.GoalRuntime()
	goal, err := store.GetGoal(context.Background(), result.GoalID)
	if err != nil || *goal.Contract.Criteria[0].Content != "required" {
		t.Fatalf("contract=%+v err=%v", goal.Contract, err)
	}
}

func goalStartCall(id string, contract agentgoal.Contract) *schema.Message {
	return testToolCall(id, agentgoal.StartToolName, contract)
}

func testToolCall(id, name string, args any) *schema.Message {
	raw, _ := json.Marshal(args)
	return schema.AssistantMessage("", []schema.ToolCall{{ID: id, Type: "function", Function: schema.FunctionCall{Name: name, Arguments: string(raw)}}})
}

func fileContract(path, content string) agentgoal.Contract {
	return agentgoal.Contract{Objective: "Set the requested file contents", Scope: "Only " + path, Criteria: []agentgoal.Criterion{{Description: "File exactly matches the requested text", Kind: "file_content", Path: path, Content: &content}}}
}

func workspaceBindings(t *testing.T) (string, []tool.Binding) {
	t.Helper()
	root := t.TempDir()
	workspace, err := codingtools.New(root)
	if err != nil {
		t.Fatal(err)
	}
	return root, workspace.Bindings()
}

func TestAutoOrdinaryQuestionsAndReadToolsDoNotCreateGoals(t *testing.T) {
	for _, input := range []string{"你好", "解释一下什么是接口", "为什么一直在 thinking？只解释不要修改", "读取配置并告诉我里面的超时是多少"} {
		t.Run(input, func(t *testing.T) {
			root, bindings := workspaceBindings(t)
			if err := os.WriteFile(filepath.Join(root, "config.txt"), []byte("timeout=30s"), 0600); err != nil {
				t.Fatal(err)
			}
			chatModel := &goalLoopModel{responses: []*schema.Message{
				testToolCall("read", "workspace_read", map[string]any{"path": "config.txt"}),
				schema.AssistantMessage("配置为 30 秒", nil),
			}}
			conversation := newGoalConversation(t, chatModel, agentgoal.Config{Enabled: true}, nil, bindings...)
			result, err := conversation.RunTurn(context.Background(), input, func(_ context.Context, event agentruntime.Event) error {
				if event.Type == agentruntime.EventGoalPhase {
					t.Errorf("ordinary question entered task phase: %+v", event.GoalPhase)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if result.GoalID != "" || chatModel.callCount() != 2 {
				t.Fatalf("result=%+v calls=%d", result, chatModel.callCount())
			}
			reopened, err := Open(context.Background(), conversation.factory, conversation.ID())
			if err != nil || reopened.CompletedTurns() != 1 {
				t.Fatalf("reopen=%v error=%v", reopened, err)
			}
		})
	}
}

func TestAutoFileTaskUsesSameTurnAndMechanicalAcceptanceWithoutModelReview(t *testing.T) {
	_, bindings := workspaceBindings(t)
	chatModel := &goalLoopModel{responses: []*schema.Message{
		goalStartCall("start", fileContract("answer.txt", "done")),
		testToolCall("write", "workspace_write", map[string]any{"path": "answer.txt", "content": "done"}),
		goalReportCall("report", agentgoal.DecisionComplete, "requested content written"),
		schema.AssistantMessage("written and verified", nil),
	}}
	conversation := newGoalConversation(t, chatModel, agentgoal.Config{Enabled: true}, nil, bindings...)
	var phases []string
	result, err := conversation.RunTurn(context.Background(), "将 answer.txt 内容设置为 done", func(_ context.Context, event agentruntime.Event) error {
		if event.GoalPhase != nil {
			phases = append(phases, event.GoalPhase.Stage)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.GoalState != agentgoal.StateCompleted || result.GoalRounds != 1 || chatModel.callCount() != 4 {
		t.Fatalf("result=%+v calls=%d", result, chatModel.callCount())
	}
	if strings.Join(phases, ",") != "executing,verifying,stopped" {
		t.Fatalf("phases=%v", phases)
	}
	_, store, _, _ := conversation.factory.GoalRuntime()
	goal, round, found, err := store.GoalForTurn(context.Background(), conversation.ID(), result.TurnID)
	if err != nil || !found || goal.Contract == nil || round.TurnID != result.TurnID || goal.Text != "将 answer.txt 内容设置为 done" {
		t.Fatalf("goal=%+v round=%+v found=%v err=%v", goal, round, found, err)
	}
}

func TestAutoFailedAcceptanceContinuesOnlyUntilCheckPasses(t *testing.T) {
	_, bindings := workspaceBindings(t)
	chatModel := &goalLoopModel{responses: []*schema.Message{
		goalStartCall("start", fileContract("answer.txt", "correct")),
		testToolCall("wrong", "workspace_write", map[string]any{"path": "answer.txt", "content": "wrong"}),
		goalReportCall("report-1", agentgoal.DecisionComplete, "claims done"),
		schema.AssistantMessage("unverified draft", nil),
		testToolCall("correct", "workspace_write", map[string]any{"path": "answer.txt", "content": "correct"}),
		goalReportCall("report-2", agentgoal.DecisionComplete, "corrected"),
		schema.AssistantMessage("accepted", nil),
	}}
	conversation := newGoalConversation(t, chatModel, agentgoal.Config{Enabled: true}, nil, bindings...)
	var phases []string
	var visible strings.Builder
	result, err := conversation.RunTurn(context.Background(), "将 answer.txt 设置为 correct", func(_ context.Context, event agentruntime.Event) error {
		if event.GoalPhase != nil {
			phases = append(phases, event.GoalPhase.Stage)
		}
		visible.WriteString(event.Delta)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.GoalRounds != 2 || result.GoalState != agentgoal.StateCompleted || chatModel.callCount() != 7 || strings.Contains(visible.String(), "unverified draft") {
		t.Fatalf("result=%+v calls=%d visible=%q", result, chatModel.callCount(), visible.String())
	}
	if strings.Join(phases, ",") != "executing,verifying,retrying,executing,verifying,stopped" {
		t.Fatalf("phases=%v", phases)
	}
}

func TestReadOnlyCoverageTaskUsesSemanticAssessment(t *testing.T) {
	chatModel := &goalLoopModel{responses: []*schema.Message{
		goalStartCall("start", semanticContract("Check every configuration entry and document coverage")),
		goalReportCall("report", agentgoal.DecisionComplete, "coverage listed"),
		schema.AssistantMessage("coverage report", nil),
	}}
	calls := 0
	evaluator := agentgoal.EvaluatorFunc(func(_ context.Context, evaluation agentgoal.Evaluation) (agentgoal.Verdict, error) {
		calls++
		if evaluation.Goal.Contract == nil {
			t.Fatal("contract was lost")
		}
		return agentgoal.Verdict{Decision: agentgoal.DecisionComplete, Reason: "fixture coverage verified"}, nil
	})
	conversation := newGoalConversationWithEvaluator(t, chatModel, agentgoal.Config{Enabled: true}, nil, evaluator)
	result, err := conversation.RunTurn(context.Background(), "逐项检查全部配置，给出完整覆盖报告，不要修改", nil)
	if err != nil || calls != 1 || result.GoalRounds != 1 || result.GoalState != agentgoal.StateCompleted {
		t.Fatalf("result=%+v assessment calls=%d err=%v", result, calls, err)
	}
}

func TestAssessmentTimeoutStopsWithoutRepeatingTask(t *testing.T) {
	chatModel := &goalLoopModel{responses: []*schema.Message{
		goalStartCall("start", semanticContract("Verify task")),
		goalReportCall("report", agentgoal.DecisionComplete, "done"),
		schema.AssistantMessage("candidate", nil),
	}}
	evaluator := agentgoal.EvaluatorFunc(func(ctx context.Context, _ agentgoal.Evaluation) (agentgoal.Verdict, error) {
		<-ctx.Done()
		return agentgoal.Verdict{}, ctx.Err()
	})
	conversation := newGoalConversationWithEvaluator(t, chatModel, agentgoal.Config{Enabled: true, EvaluationTimeout: 20 * time.Millisecond}, nil, evaluator)
	verifying := false
	result, err := conversation.RunTurn(context.Background(), "完成并验证任务", func(_ context.Context, event agentruntime.Event) error {
		if event.GoalPhase != nil && event.GoalPhase.Stage == "verifying" {
			verifying = true
		}
		return nil
	})
	if !errors.Is(err, context.DeadlineExceeded) || !verifying || chatModel.callCount() != 3 {
		t.Fatalf("result=%+v calls=%d verifying=%v err=%v", result, chatModel.callCount(), verifying, err)
	}
	_, store, _, _ := conversation.factory.GoalRuntime()
	goal, _, found, loadErr := store.GoalForTurn(context.Background(), conversation.ID(), result.TurnID)
	if loadErr != nil || !found || goal.State != agentgoal.StateFailed {
		t.Fatalf("goal=%+v err=%v", goal, loadErr)
	}
}
