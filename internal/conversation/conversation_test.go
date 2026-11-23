package conversation

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/schema"
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
