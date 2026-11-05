package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentgoal"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

func TestRejectedAdmissionCannotMutateReportOrCompleteAsOrdinaryChat(t *testing.T) {
	root, bindings := workspaceBindings(t)
	var executions atomic.Int32
	bindings = append(bindings, tool.Binding{
		Descriptor: tool.Descriptor{Name: "test_exec", Description: "Test execution boundary", Revision: "v1", RiskClass: tool.RiskExecute, InputSchema: json.RawMessage(`{"type":"object"}`)},
		Executor: tool.InvokeFunc(func(context.Context, json.RawMessage) (json.RawMessage, error) {
			executions.Add(1)
			return json.RawMessage(`{}`), nil
		}),
	})
	chatModel := &goalLoopModel{responses: []*schema.Message{
		goalStartCall("bad-start", agentgoal.Contract{}),
		testToolCall("write", "workspace_write", map[string]any{"path": "never.txt", "content": "wrong"}),
		testToolCall("execute", "test_exec", map[string]any{}),
		goalReportCall("report", agentgoal.DecisionComplete, "false report"),
		schema.AssistantMessage("false final completion", nil),
	}}
	conversation := newGoalConversation(t, chatModel, agentgoal.Config{Enabled: true}, nil, bindings...)
	var visible strings.Builder
	blockedResults := 0
	result, err := conversation.RunTurn(context.Background(), "写入文件", func(_ context.Context, event agentruntime.Event) error {
		visible.WriteString(event.Delta)
		if event.Message != nil {
			visible.WriteString(event.Message.Content)
			if event.Type == agentruntime.EventToolResult && strings.Contains(event.Message.Content, agentgoal.AdmissionRequiredCode) {
				blockedResults++
			}
		}
		return nil
	})
	var precondition *tool.PreconditionError
	if !errors.As(err, &precondition) || precondition.Code != agentgoal.AdmissionRequiredCode {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if _, err := os.Stat(filepath.Join(root, "never.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unadmitted task wrote a file: %v", err)
	}
	if executions.Load() != 0 || blockedResults != 3 || strings.Contains(visible.String(), "false final completion") || strings.Contains(visible.String(), "active goal round for turn") {
		t.Fatalf("executions=%d blocked=%d visible=%s", executions.Load(), blockedResults, visible.String())
	}
	if conversation.CompletedTurns() != 0 {
		t.Fatal("failed admission counted as ordinary success")
	}
	reopened, err := Open(context.Background(), conversation.factory, conversation.ID())
	if err != nil || reopened.CompletedTurns() != 0 {
		t.Fatalf("reopen=%v err=%v", reopened, err)
	}
	requested, err := conversation.factory.AdmissionRequestedForTurn(context.Background(), conversation.ID(), result.TurnID)
	if err != nil || !requested {
		t.Fatalf("pending admission was not durable: requested=%t err=%v", requested, err)
	}
}

func TestAdmissionRecoveryInSameEinoTurnWritesOnlyOnce(t *testing.T) {
	for _, initial := range []string{"invalid contract", "skipped registration"} {
		t.Run(initial, func(t *testing.T) {
			_, bindings := workspaceBindings(t)
			first := goalStartCall("bad-start", agentgoal.Contract{})
			if initial == "skipped registration" {
				first = testToolCall("early-write", "workspace_write", map[string]any{"path": "answer.txt", "content": "done"})
			}
			// Eino executes these in order within the same tool batch; visibility
			// filtering must not remove handlers needed after admission succeeds.
			batch := goalStartCall("start", fileContract("answer.txt", "done"))
			batch.ToolCalls = append(batch.ToolCalls,
				testToolCall("write", "workspace_write", map[string]any{"path": "answer.txt", "content": "done"}).ToolCalls...)
			batch.ToolCalls = append(batch.ToolCalls, goalReportCall("report", agentgoal.DecisionComplete, "done").ToolCalls...)
			chatModel := &goalLoopModel{responses: []*schema.Message{first, batch, schema.AssistantMessage("accepted", nil)}}
			conversation := newGoalConversation(t, chatModel, agentgoal.Config{Enabled: true}, nil, bindings...)
			writes := 0
			result, err := conversation.RunTurn(context.Background(), "将 answer.txt 设置为 done", func(_ context.Context, event agentruntime.Event) error {
				if event.Type == agentruntime.EventToolResult && event.Message != nil && event.Message.ToolName == "workspace_write" && !strings.Contains(event.Message.Content, `"error"`) {
					writes++
				}
				return nil
			})
			if err != nil || result.GoalState != agentgoal.StateCompleted || result.GoalRounds != 1 || writes != 1 || chatModel.callCount() != 3 {
				t.Fatalf("result=%+v writes=%d calls=%d err=%v", result, writes, chatModel.callCount(), err)
			}
			want := [][2]bool{{true, false}, {true, false}, {false, true}}
			for i, phase := range chatModel.goalTools {
				if phase != want[i] {
					t.Fatalf("model call %d goal tools=%v want=%v", i, phase, want[i])
				}
			}
		})
	}
}

func TestGitignoreSemanticContextIncidentUsesEvaluatorNotExactComparison(t *testing.T) {
	root, bindings := workspaceBindings(t)
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("/logs/\n"), 0600); err != nil {
		t.Fatal(err)
	}
	contextText := "`.gitignore` contains a new ignore entry for the `testdata` folder."
	contract := agentgoal.Contract{
		Objective: "Modify `.gitignore` by adding an ignore rule for the testdata folder.",
		Scope:     "Only modify `.gitignore`; preserve all existing entries and unrelated workspace changes.",
		Criteria:  []agentgoal.Criterion{{Description: "Add one new line to `.gitignore` that ignores the testdata directory.", Kind: "semantic", Path: ".gitignore", Content: &contextText}},
	}
	chatModel := &goalLoopModel{responses: []*schema.Message{
		goalStartCall("start", contract),
		testToolCall("edit", "workspace_edit", map[string]any{"path": ".gitignore", "old_text": "/logs/\n", "new_text": "/logs/\n/testdata/\n"}),
		goalReportCall("report", agentgoal.DecisionComplete, "requested ignore entry added"),
		schema.AssistantMessage("已添加忽略规则", nil),
	}}
	calls := 0
	evaluator := agentgoal.EvaluatorFunc(func(_ context.Context, evaluation agentgoal.Evaluation) (agentgoal.Verdict, error) {
		calls++
		got := evaluation.Goal.Contract.Criteria[0]
		if got.Kind != "semantic" || got.Path != ".gitignore" || got.Content == nil || *got.Content != contextText {
			t.Fatalf("lost semantic context: %+v", got)
		}
		actual, err := os.ReadFile(filepath.Join(root, ".gitignore"))
		if err != nil || string(actual) != "/logs/\n/testdata/\n" {
			t.Fatalf("actual=%q err=%v", actual, err)
		}
		return agentgoal.Verdict{Decision: agentgoal.DecisionComplete, Reason: "fixture verified"}, nil
	})
	conversation := newGoalConversationWithEvaluator(t, chatModel, agentgoal.Config{Enabled: true}, nil, evaluator, bindings...)
	result, err := conversation.RunTurn(context.Background(), "在 .gitignore 追加 /testdata/", nil)
	if err != nil || calls != 1 || result.GoalState != agentgoal.StateCompleted || chatModel.callCount() != 4 {
		t.Fatalf("result=%+v evaluations=%d err=%v", result, calls, err)
	}
}
