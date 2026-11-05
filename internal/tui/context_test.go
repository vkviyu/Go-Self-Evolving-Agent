package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/modelcontext"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenizer"
)

func TestContextCommandExplainsCapacityAndWorkingBudget(t *testing.T) {
	model := newTestModel(t)
	model.contextUsage = agentruntime.ContextUsage{
		Used: 132000, Window: 1050000, Trigger: 200000, WindowSource: modelcontext.SourceConfigured,
		InputLimit: 1037712, OutputReserve: 8192, Status: tokenizer.StatusProvider, State: "ready",
		Capacity: modelcontext.Metadata{Window: 1050000, Source: modelcontext.SourceCatalog, MaxOutputTokens: 128000, OutputSource: modelcontext.SourceCatalog},
	}
	if _, handled := model.handleCommand("/context"); !handled {
		t.Fatal("context command not handled")
	}
	plain := ansi.Strip(model.transcriptContent)
	for _, want := range []string{
		"Full input: 132000 / 1037712", "Working window: 1050000 tokens (configured)",
		"Model capacity: 1050000 tokens (catalog)", "Max output: 128000 tokens (catalog)",
		"Input budget: 1037712", "Auto-compaction trigger: 200000", "Output reserve: 8192",
		"State: ready", "Input includes system instructions and tool schemas", "Cached input still occupies context",
	} {
		if !strings.Contains(plain, want) {
			t.Errorf("context view missing %q: %s", want, plain)
		}
	}
}

func TestContextCommandExplainsUnknownCapacityAndDeferredCompaction(t *testing.T) {
	model := newTestModel(t)
	model.contextUsage = agentruntime.ContextUsage{
		Used: 25000, Window: 32768, Trigger: 20000, InputLimit: 22938, OutputReserve: 8192,
		WindowSource: modelcontext.SourceFallback, Status: tokenizer.StatusEstimated,
		Capacity: modelcontext.Metadata{Window: 32768, Source: modelcontext.SourceFallback},
		State:    "over_budget", NeedsCompaction: true,
	}
	model.handleCommand("/context")
	plain := ansi.Strip(model.transcriptContent)
	for _, want := range []string{
		"Working window: 32768 tokens (fallback)",
		"Model capacity: unknown (fallback reference: 32768 tokens; not a known model capacity)",
		"Max output: unknown (not declared)", "State: over_budget",
		"before the next model request", "No idle model request is made",
	} {
		if !strings.Contains(plain, want) {
			t.Errorf("context view missing %q: %s", want, plain)
		}
	}
}

func TestContextStatusUsesInputBudgetAndCompletionState(t *testing.T) {
	for _, test := range []struct {
		state   agentruntime.ContextState
		pending bool
		hint    string
	}{
		{"ready", false, ""},
		{"compaction_pending", true, "compact pending"},
		{"over_budget", true, "over budget"},
	} {
		t.Run(string(test.state), func(t *testing.T) {
			model := newTestModel(t)
			model.contextUsage = agentruntime.ContextUsage{
				Used: 12000, Window: 32768, InputLimit: 22938, Trigger: 18000,
				State: test.state, NeedsCompaction: test.pending, Status: tokenizer.StatusEstimated,
			}
			status := ansi.Strip(model.renderStatus(160))
			if !strings.Contains(status, "ctx input 12k / 22k") || strings.Contains(status, "/ 32k") {
				t.Fatalf("status does not use the full-input budget: %q", status)
			}
			if test.hint != "" && !strings.Contains(status, test.hint) {
				t.Fatalf("status omits %q: %q", test.hint, status)
			}
			if test.hint == "" && (strings.Contains(status, "pending") || strings.Contains(status, "over budget")) {
				t.Fatalf("ready context has a pending warning: %q", status)
			}
		})
	}
}

type contextUsageConversation struct {
	fakeConversation
	usage        agentruntime.ContextUsage
	refreshCount int
}

func (conversation *contextUsageConversation) ContextUsage() agentruntime.ContextUsage {
	conversation.refreshCount++
	return conversation.usage
}

func (conversation *contextUsageConversation) Compress(context.Context) (bool, agentruntime.ContextUsage, error) {
	return false, conversation.usage, nil
}

func TestFinishTurnPrefersCompletedTurnContextUsage(t *testing.T) {
	for _, withResultUsage := range []bool{false, true} {
		conversation := &contextUsageConversation{usage: agentruntime.ContextUsage{Used: 1000, Window: 32768, InputLimit: 22938, State: "ready"}}
		model := newTestModel(t)
		model.conversation = conversation
		result := agentruntime.Result{Final: schema.AssistantMessage("answer", nil)}
		if withResultUsage {
			result.ContextUsage = &agentruntime.ContextUsage{
				Used: 21000, Window: 32768, InputLimit: 22938, Trigger: 20000,
				State: "compaction_pending", NeedsCompaction: true,
			}
		}
		model.finishTurn(turnDoneMsg{result: result})
		if conversation.refreshCount != 1 {
			t.Fatalf("history usage was not refreshed exactly once: %d", conversation.refreshCount)
		}
		wantUsed := conversation.usage.Used
		if withResultUsage {
			wantUsed = result.ContextUsage.Used
		}
		if model.contextUsage.Used != wantUsed || model.contextUsage.NeedsCompaction != withResultUsage {
			t.Fatalf("completed context was not preferred: result=%v usage=%+v", withResultUsage, model.contextUsage)
		}
	}
}
