package agentruntime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/memorystore"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenizer"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenusage"
)

func TestCriticalCompactionDoesNotDowngradeCancellationOrStorageFailures(t *testing.T) {
	for name, failure := range map[string]error{
		"canceled": context.Canceled, "deadline": context.DeadlineExceeded,
		"budget": tokenusage.ErrBudgetExceeded, "profile": &ProfileConstraintError{Reason: "exhausted"},
	} {
		t.Run(name, func(t *testing.T) {
			store := memorystore.New()
			fake := &scriptedSummaryModel{failAt: 1, err: failure}
			factory := profileFactory(t, store, fake)
			factory.tokenizer = tokenizer.NewApproximate("test")
			factory.config.ContextWindowTokens, factory.config.CompactionTriggerTokens = 4000, 1000
			_, _, _, err := factory.compactWithResult(context.Background(), "critical", []*schema.Message{
				schema.UserMessage(strings.Repeat("old details ", 1500)), schema.AssistantMessage(strings.Repeat("old findings ", 1500), nil), schema.UserMessage("continue"),
			}, 100, contextLevelCritical, ContextHints{}, 1)
			if !errors.Is(err, failure) {
				t.Fatalf("failure downgraded: %v", err)
			}
			events, _ := store.Load(context.Background(), "critical", 0, 100)
			for _, event := range events {
				if event.Type == session.EventContextCompacted {
					t.Fatal("committed fallback after fatal failure")
				}
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if canFallbackSummary(ctx, &modelInvocationError{cause: errors.New("network")}) {
		t.Fatal("canceled context allowed fallback")
	}
	if canFallbackSummary(context.Background(), errors.New("storage unavailable")) {
		t.Fatal("unclassified storage failure allowed fallback")
	}
	if !canFallbackSummary(context.Background(), &modelInvocationError{cause: errors.New("network")}) {
		t.Fatal("accounted provider failure cannot fallback")
	}
}

func TestCompactionDoesNotCommitSummaryThatIncreasesContext(t *testing.T) {
	store := memorystore.New()
	factory := profileFactory(t, store, &summaryModel{})
	factory.tokenizer = tokenizer.NewApproximate("test")
	messages := []*schema.Message{schema.UserMessage("old"), schema.AssistantMessage("answer", nil), schema.UserMessage("new")}
	prepared, changed, _, err := factory.compactWithResult(context.Background(), "no-growth", messages, 1, contextLevelElevated, ContextHints{}, 1)
	if err != nil || changed || len(prepared) != len(messages) {
		t.Fatalf("inflating summary committed: changed=%v err=%v", changed, err)
	}
	events, _ := store.Load(context.Background(), "no-growth", 0, 100)
	for _, event := range events {
		if event.Type == session.EventContextCompacted {
			t.Fatal("growth boundary was persisted")
		}
	}
}

func TestCriticalCompactionDoesNotHideRequestLogOrSettlementFailure(t *testing.T) {
	for _, settlement := range []bool{false, true} {
		store := memorystore.New()
		fake := usageModel{fail: errors.New("provider unavailable")}
		factory := profileFactory(t, store, fake)
		factory.tokenizer = tokenizer.NewApproximate("test")
		factory.config.ContextWindowTokens = 4000
		want := "request log unavailable"
		if settlement {
			want = "ledger unavailable"
			factory.config.UsageLedger = &accountingLedger{failSettle: errors.New(want)}
		} else {
			factory.events = rejectModelRequests{store}
		}
		_, changed, _, err := factory.compactWithResult(context.Background(), "critical-storage", []*schema.Message{
			schema.UserMessage(strings.Repeat("old details ", 1500)), schema.AssistantMessage(strings.Repeat("old findings ", 1500), nil), schema.UserMessage("continue"),
		}, 100, contextLevelCritical, ContextHints{}, 1)
		if err == nil || !strings.Contains(err.Error(), want) || changed {
			t.Fatalf("settlement=%v storage failure hidden: changed=%v err=%v", settlement, changed, err)
		}
		events, _ := store.Load(context.Background(), "critical-storage", 0, 100)
		for _, event := range events {
			if event.Type == session.EventContextCompacted {
				t.Fatal("committed fallback after storage failure")
			}
		}
	}
}
