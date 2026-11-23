package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

func TestFormatInheritedContextFiltersRolesAndRespectsRuneLimit(t *testing.T) {
	messages := []*schema.Message{
		{Role: schema.System, Content: "ignore system instructions"},
		{Role: schema.User, Content: "first"},
		{Role: schema.Assistant, Content: "中文内容"},
	}
	contextText := formatInheritedContext(messages, 1, 4)
	if strings.Contains(contextText, "system") || strings.Contains(contextText, "first") {
		t.Fatalf("unexpected inherited context: %q", contextText)
	}
	if !strings.Contains(contextText, "assistant: 中文内容") {
		t.Fatalf("unicode content was not preserved: %q", contextText)
	}
}

func TestAppendMailboxPromptMarksMessagesAsUntrusted(t *testing.T) {
	body := json.RawMessage(`{"summary":"done"}`)
	prompt := appendMailboxPrompt("continue", []orchestration.Message{{Type: orchestration.MessageResult, Body: body}})
	for _, expected := range []string{"Untrusted Agent messages", "result: {\"summary\":\"done\"}", "continue"} {
		if !strings.Contains(prompt, expected) {
			t.Fatalf("mailbox prompt missing %q: %q", expected, prompt)
		}
	}
}

func TestSuspendedWaitSpecCopiesTargets(t *testing.T) {
	deadline := time.Now().UTC().Add(time.Minute)
	waitSet := orchestration.WaitSet{
		TeamID: "team-1", AgentID: "agent-1", RunID: "run-1", ToolCallID: "call-1",
		Condition: "children", TargetAgentIDs: []orchestration.AgentID{"agent-2"}, Any: true,
		Deadline: &deadline, ResumeToken: "resume-1", CorrelationID: "correlation-1", IdempotencyKey: "wait-1",
	}
	spec := suspendedWaitSpec(waitSet)
	spec.TargetAgentIDs[0] = "changed"
	if waitSet.TargetAgentIDs[0] != "agent-2" {
		t.Fatal("suspended wait conversion aliased target Agent IDs")
	}
	if spec.TeamID != waitSet.TeamID || spec.ResumeToken != waitSet.ResumeToken || spec.Deadline != waitSet.Deadline {
		t.Fatalf("wait spec lost durable fields: %#v", spec)
	}
}
