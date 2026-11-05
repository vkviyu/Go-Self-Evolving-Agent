package conversation

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
)

func TestRunTurnRejectsEmptyInput(t *testing.T) {
	conversation := &Conversation{}
	_, err := conversation.RunTurn(context.Background(), "   ", nil)
	if err == nil || err.Error() != "conversation input must not be empty" {
		t.Fatalf("error = %v", err)
	}
}

func TestHistoryReturnsDeepCopy(t *testing.T) {
	t.Parallel()

	original := schema.AssistantMessage("answer", []schema.ToolCall{{
		ID:   "call-1",
		Type: "function",
		Function: schema.FunctionCall{
			Name:      "workspace_read",
			Arguments: `{"path":"README.md"}`,
		},
	}})
	conversation := &Conversation{history: []*schema.Message{original}}
	snapshot := conversation.History()
	if len(snapshot) != 1 || snapshot[0] == nil {
		t.Fatalf("unexpected history snapshot: %#v", snapshot)
	}
	snapshot[0].Content = "mutated"
	snapshot[0].ToolCalls[0].Function.Arguments = `{"path":"other.txt"}`
	if original.Content != "answer" || original.ToolCalls[0].Function.Arguments != `{"path":"README.md"}` {
		t.Fatalf("history snapshot shares mutable message data: %#v", original)
	}
}

func TestCandidateEventBufferSuppressesContentOnToolMessage(t *testing.T) {
	original := schema.AssistantMessage("draft explanation", []schema.ToolCall{{
		ID:   "call-1",
		Type: "function",
		Function: schema.FunctionCall{
			Name:      "goal_report",
			Arguments: `{"status":"complete"}`,
		},
	}})
	var received agentruntime.Event
	buffer := &candidateEventBuffer{downstream: func(_ context.Context, event agentruntime.Event) error {
		received = event
		return nil
	}}

	if err := buffer.Sink(context.Background(), agentruntime.Event{
		Type:    agentruntime.EventAssistantMessage,
		Message: original,
	}); err != nil {
		t.Fatal(err)
	}
	if received.Message == nil {
		t.Fatal("tool message was not forwarded")
	}
	if received.Message.Content != "" {
		t.Fatalf("forwarded candidate content = %q, want it suppressed", received.Message.Content)
	}
	if len(received.Message.ToolCalls) != 1 || received.Message.ToolCalls[0].Function.Name != "goal_report" {
		t.Fatalf("forwarded tool calls = %#v", received.Message.ToolCalls)
	}
	if original.Content != "draft explanation" {
		t.Fatalf("candidate buffer mutated original message content = %q", original.Content)
	}
}
