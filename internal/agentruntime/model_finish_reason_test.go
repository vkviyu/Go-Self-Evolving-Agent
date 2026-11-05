package agentruntime

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/memorystore"
)

func TestAssistantReceiptKeepsTerminalReasonWithoutInventingUsage(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, reason := range []string{"stop", "completed", "length", ""} {
			store := memorystore.New()
			recorded := &recordingModel{events: store, sessionID: "finish", turnID: "turn"}
			message := schema.AssistantMessage("完整的最后一句。", nil)
			if reason != "" {
				message.ResponseMeta = &schema.ResponseMeta{FinishReason: reason}
			}
			if err := recorded.recordAssistantMessage(context.Background(), message, streaming, "request"); err != nil {
				t.Fatal(err)
			}
			events, err := store.Load(context.Background(), "finish", 0, 10)
			if err != nil || len(events) != 1 {
				t.Fatalf("events=%v err=%v", events, err)
			}
			var payload map[string]any
			if err := json.Unmarshal(events[0].Payload, &payload); err != nil {
				t.Fatal(err)
			}
			got, present := payload["finish_reason"]
			if (reason != "" && got != reason) || (reason == "" && present) || payload["content"] != message.Content || payload["usage_quality"] != "estimated_reservation" {
				t.Fatalf("reason=%q streaming=%t payload=%#v", reason, streaming, payload)
			}
			if _, present := payload["total_tokens"]; present {
				t.Fatal("missing provider usage was replaced by invented tokens")
			}
		}
	}
}

func TestProviderTerminalReasonSurvivesHistoryAndTranscript(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, providerUsage := range []bool{false, true} {
			ctx, store := context.Background(), memorystore.New()
			message := schema.AssistantMessage("完整的最后一句。", nil)
			message.ResponseMeta = &schema.ResponseMeta{FinishReason: "completed"}
			if providerUsage {
				message.ResponseMeta.Usage = &schema.TokenUsage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12}
			}
			fake := usageModel{response: message}
			factory := profileFactory(t, store, fake)
			recorded := &recordingModel{factory: factory, inner: fake, events: store, sessionID: "finish", turnID: "turn"}
			if _, err := store.Append(ctx, "finish", session.AnySequence, session.PendingEvent{
				Type: session.EventUserMessage, CorrelationID: "turn", Payload: session.MustJSONPayload(map[string]string{"content": "hello"}),
			}); err != nil {
				t.Fatal(err)
			}
			if streaming {
				stream, err := recorded.Stream(ctx, []*schema.Message{schema.UserMessage("hello")})
				if err != nil {
					t.Fatal(err)
				}
				if err := drainModelStream(stream); err != nil {
					t.Fatal(err)
				}
			} else if _, err := recorded.Generate(ctx, []*schema.Message{schema.UserMessage("hello")}); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Append(ctx, "finish", session.AnySequence, session.PendingEvent{
				Type: session.EventTurnCompleted, CorrelationID: "turn", Payload: session.MustJSONPayload(map[string]string{"content": message.Content}),
			}); err != nil {
				t.Fatal(err)
			}
			for _, load := range []func(context.Context, session.ID) ([]*schema.Message, error){factory.LoadHistory, factory.LoadTranscript} {
				history, err := load(ctx, "finish")
				if err != nil || len(history) != 2 {
					t.Fatalf("history=%v err=%v", history, err)
				}
				answer := history[1]
				if answer.Content != message.Content || answer.ResponseMeta == nil || answer.ResponseMeta.FinishReason != "completed" {
					t.Fatalf("lost terminal receipt: %#v", answer)
				}
				if (answer.ResponseMeta.Usage != nil) != providerUsage {
					t.Fatalf("invented or lost usage: %#v", answer.ResponseMeta)
				}
				if providerUsage && *answer.ResponseMeta.Usage != *message.ResponseMeta.Usage {
					t.Fatalf("usage changed: %#v", answer.ResponseMeta.Usage)
				}
			}
		}
	}
}
