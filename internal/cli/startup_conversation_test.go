package cli

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	conversationapi "github.com/vkviyu/go-self-evolving-agent/internal/conversation"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

type startupConversationFake struct {
	runs      atomic.Int32
	resumedID string
}

func (*startupConversationFake) ID() session.ID { return "hydrated" }
func (*startupConversationFake) History() []*schema.Message {
	return []*schema.Message{schema.UserMessage("history")}
}
func (*startupConversationFake) CompletedTurns() int { return 1 }
func (base *startupConversationFake) RunTurn(ctx context.Context, input string, sink agentruntime.EventSink) (agentruntime.Result, error) {
	base.runs.Add(1)
	if sink != nil {
		if err := sink(ctx, agentruntime.Event{Type: agentruntime.EventAssistantTextDelta, Delta: input}); err != nil {
			return agentruntime.Result{}, err
		}
	}
	return agentruntime.Result{Final: schema.AssistantMessage(input, nil)}, nil
}
func (*startupConversationFake) ContextUsage() agentruntime.ContextUsage {
	return agentruntime.ContextUsage{Used: 10}
}
func (*startupConversationFake) ContextUsageFor(input string) agentruntime.ContextUsage {
	return agentruntime.ContextUsage{Used: 10 + len(input)}
}
func (*startupConversationFake) Compress(context.Context) (bool, agentruntime.ContextUsage, error) {
	return true, agentruntime.ContextUsage{Used: 3}, nil
}
func (*startupConversationFake) Recoveries(context.Context) ([]conversationapi.Recovery, error) {
	return []conversationapi.Recovery{{TurnID: "interrupted", Phase: "execution"}}, nil
}
func (base *startupConversationFake) ResumeTurn(ctx context.Context, id string, sink agentruntime.EventSink) (agentruntime.Result, error) {
	base.resumedID = id
	return base.RunTurn(ctx, id, sink)
}

func TestStartupConversationWaitsForMaintenanceBeforeCallingBase(t *testing.T) {
	ready := make(chan struct{})
	base := &startupConversationFake{}
	conversation := &startupConversation{base: pendingConversation{id: "pending"}, ready: ready}
	done := make(chan error, 1)
	events := make(chan agentruntime.Event, 1)
	go func() {
		_, err := conversation.RunTurn(t.Context(), "question", func(_ context.Context, event agentruntime.Event) error {
			events <- event
			return nil
		})
		done <- err
	}()
	// Even attaching the base must not release the mandatory recovery barrier.
	conversation.setBase(base)
	select {
	case err := <-done:
		t.Fatalf("turn completed before maintenance: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	if base.runs.Load() != 0 {
		t.Fatal("model request ran before startup readiness")
	}
	close(ready)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("turn did not pass the completed startup barrier")
	}
	if event := <-events; event.Type != agentruntime.EventAssistantTextDelta || event.Delta != "question" {
		t.Fatalf("event was not forwarded: %+v", event)
	}
	if conversation.ID() != "hydrated" || len(conversation.History()) != 1 || conversation.CompletedTurns() != 1 {
		t.Fatal("base history was not exposed after attachment")
	}
	if usage := conversation.ContextUsageFor("pending"); usage.Used != 17 {
		t.Fatalf("pending context usage was not forwarded: %+v", usage)
	}
	items, err := conversation.Recoveries(t.Context())
	if err != nil || len(items) != 1 || items[0].TurnID != "interrupted" {
		t.Fatalf("recoveries not forwarded: %v %v", items, err)
	}
	if _, err := conversation.ResumeTurn(t.Context(), "interrupted", nil); err != nil || base.resumedID != "interrupted" {
		t.Fatalf("resume not forwarded: %s %v", base.resumedID, err)
	}
}

func TestStartupConversationFailureAndCancellationBlockCapabilities(t *testing.T) {
	startupErr := errors.New("mandatory maintenance failed")
	ready := make(chan struct{})
	base := &startupConversationFake{}
	conversation := &startupConversation{
		base: base, ready: ready, readyErr: func() error { return startupErr },
	}
	close(ready)
	for name, run := range map[string]func(context.Context) error{
		"turn":       func(ctx context.Context) error { _, err := conversation.RunTurn(ctx, "", nil); return err },
		"resume":     func(ctx context.Context) error { _, err := conversation.ResumeTurn(ctx, "turn", nil); return err },
		"recoveries": func(ctx context.Context) error { _, err := conversation.Recoveries(ctx); return err },
		"compress":   func(ctx context.Context) error { _, _, err := conversation.Compress(ctx); return err },
		"pending":    func(ctx context.Context) error { _, err := conversation.PendingBackground(ctx); return err },
		"background": func(ctx context.Context) error { _, err := conversation.RunBackgroundTurn(ctx, nil); return err },
	} {
		t.Run(name, func(t *testing.T) {
			if err := run(t.Context()); !errors.Is(err, startupErr) {
				t.Fatalf("startup failure was bypassed: %v", err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if err := run(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled operation crossed ready barrier: %v", err)
			}
		})
	}
	if base.runs.Load() != 0 || base.resumedID != "" {
		t.Fatal("failed startup invoked the base")
	}
}
