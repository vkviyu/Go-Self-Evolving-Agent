package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/modelcontext"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/memorystore"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenusage"
)

func TestPrepareMessagesDoesNotReadTeamBudgetBeforeContextThreshold(t *testing.T) {
	for _, state := range contextPolicyBudgetStates() {
		t.Run(state.name, func(t *testing.T) {
			store, fake := memorystore.New(), &scriptedSummaryModel{responses: []string{testSemanticSummary}}
			reader := &contextPolicyBudgetReader{snapshot: state.snapshot, err: state.err}
			factory := contextBudgetFactory(t, store, fake, reader)
			// 360 tokens exceed half the window but remain below the 450-token
			// trigger. An exhausted snapshot without a UsageLedger is exercised
			// only here: this is a policy check, not model admission.
			messages := contextBudgetMessages(12)
			prepared, err := factory.PrepareMessages(context.Background(), "below-trigger", messages)
			if err != nil || !reflect.DeepEqual(prepared, messages) {
				t.Fatalf("team budget changed below-trigger context: prepared=%+v err=%v", prepared, err)
			}
			if reader.calls != 0 || len(fake.inputs) != 0 {
				t.Fatalf("below-trigger policy read budget or requested summary: reads=%d summaries=%d", reader.calls, len(fake.inputs))
			}
			events, err := store.Load(context.Background(), "below-trigger", 0, 100)
			if err != nil || len(events) != 0 {
				t.Fatalf("unchanged context wrote events: events=%+v err=%v", events, err)
			}
		})
	}
}

func TestContextCompactionShapeDoesNotReadTeamBudget(t *testing.T) {
	for _, mode := range []struct {
		name, level  string
		force        bool
		messages     int
		retain, keep int
	}{
		{name: "automatic at trigger", level: "elevated", messages: 15, retain: 150, keep: 5},
		{name: "manual below trigger", level: "normal", force: true, messages: 12, retain: 200, keep: 6},
	} {
		t.Run(mode.name, func(t *testing.T) {
			for _, state := range contextPolicyBudgetStates() {
				if state.snapshot.Exhausted {
					continue // Actual exhaustion is covered by the refusing ledger below.
				}
				t.Run(state.name, func(t *testing.T) {
					ctx, store := context.Background(), memorystore.New()
					fake := &scriptedSummaryModel{responses: []string{testSemanticSummary}}
					reader := &contextPolicyBudgetReader{snapshot: state.snapshot, err: state.err}
					factory := contextBudgetFactory(t, store, fake, reader)
					factory.config.UsageLedger = &accountingLedger{}
					messages := contextBudgetMessages(mode.messages)
					var prepared []*schema.Message
					var err error
					if mode.force {
						var changed bool
						prepared, changed, err = factory.Compact(ctx, "same-context", messages)
						if !changed {
							t.Fatal("manual compaction did not commit a reducing boundary")
						}
					} else {
						prepared, err = factory.PrepareMessages(ctx, "same-context", messages)
					}
					if err != nil {
						t.Fatal(err)
					}
					if len(prepared) != mode.keep+1 || prepared[0].Role != schema.System ||
						!strings.HasPrefix(prepared[0].Content, "Conversation summary:\n") ||
						!reflect.DeepEqual(prepared[1:], messages[len(messages)-mode.keep:]) {
						t.Fatalf("team budget changed retained original messages: prepared=%+v", prepared)
					}
					if reader.calls != 0 || len(fake.inputs) != 1 {
						t.Fatalf("policy read budget or changed summary attempts: reads=%d summaries=%d", reader.calls, len(fake.inputs))
					}
					common := model.GetCommonOptions(nil, fake.options[0]...)
					if common.MaxTokens == nil || *common.MaxTokens != 100 {
						t.Fatalf("team budget changed summary output allowance: %+v", common)
					}
					if used, limit := factory.tokenizer.CountMessages(fake.inputs[0]), modelcontext.InputLimit(600, *common.MaxTokens); used > limit {
						t.Fatalf("summary fixture exceeds request capacity: %d > %d", used, limit)
					}
					events, err := store.Load(ctx, "same-context", 0, 100)
					if err != nil {
						t.Fatal(err)
					}
					compacted, managed := 0, 0
					for _, event := range events {
						if event.Type != session.EventContextCompacted && event.Type != session.EventContextManaged {
							continue
						}
						var payload map[string]json.RawMessage
						if err := json.Unmarshal(event.Payload, &payload); err != nil {
							t.Fatal(err)
						}
						if string(payload["policy_level"]) != fmt.Sprintf("%q", mode.level) || string(payload["fallback"]) != "false" {
							t.Fatalf("team budget changed policy level or fallback: %s", event.Payload)
						}
						if event.Type == session.EventContextCompacted {
							compacted++
							if string(payload["retain_tokens"]) != fmt.Sprint(mode.retain) {
								t.Fatalf("team budget changed retention allowance: %s", event.Payload)
							}
						} else {
							managed++
							if _, present := payload["budget_pressure"]; present {
								t.Fatalf("new context management event retains budget pressure: %s", event.Payload)
							}
						}
					}
					if compacted != 1 || managed != 1 {
						t.Fatalf("missing durable compaction/management: compacted=%d managed=%d", compacted, managed)
					}
				})
			}
		})
	}
}

func TestSummaryReservationRefusalRemainsFatalAtCriticalContext(t *testing.T) {
	ctx, store := context.Background(), memorystore.New()
	fake := &scriptedSummaryModel{responses: []string{testSemanticSummary}}
	reader := &contextPolicyBudgetReader{snapshot: tokenusage.BudgetSnapshot{Exhausted: true}}
	factory := contextBudgetFactory(t, store, fake, reader)
	ledger := &refusingContextReservationLedger{}
	factory.config.UsageLedger = ledger
	// 540 tokens is critical in this window, where ordinary provider failures
	// may use extractive fallback. A real reservation refusal must stay fatal.
	prepared, err := factory.PrepareMessages(ctx, "hard-budget", contextBudgetMessages(18))
	if !errors.Is(err, tokenusage.ErrBudgetExceeded) || prepared != nil {
		t.Fatalf("reservation refusal became successful compaction: prepared=%+v err=%v", prepared, err)
	}
	if reader.calls != 0 || len(fake.inputs) != 0 || len(ledger.requests) != 1 {
		t.Fatalf("refused request dispatched or skipped ledger: budget reads=%d provider calls=%d reservations=%d", reader.calls, len(fake.inputs), len(ledger.requests))
	}
	request := ledger.requests[0]
	if request.TeamID != "team-context-budget" || request.InputTokens != 60 || request.OutputTokens != 100 {
		t.Fatalf("summary did not reserve its bounded team request: %+v", request)
	}
	if ledger.settlementCalls != 0 || len(ledger.records) != 0 || ledger.released != 0 {
		t.Fatalf("refused reservation was settled or refunded: %+v", ledger.accountingLedger)
	}
	events, err := store.Load(ctx, "hard-budget", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == session.EventContextCompacted || event.Type == session.EventContextManaged || event.Type == session.EventModelRequested {
			t.Fatalf("refused summary recorded dispatch or successful fallback: %+v", event)
		}
	}
}

func TestProfileOutputBudgetPreservesPlannedSummaryAllowance(t *testing.T) {
	for _, test := range []struct {
		name  string
		used  int
		allow bool
	}{
		{name: "insufficient remaining output", used: 100},
		{name: "exact remaining output", used: 50, allow: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, store := context.Background(), memorystore.New()
			fake := &scriptedSummaryModel{responses: []string{testSemanticSummary}}
			parent := contextBudgetFactory(t, store, fake, &contextPolicyBudgetReader{})
			profile := runtimeTestProfile()
			profile.Limits.MaxOutputTokens = 150
			factory, err := parent.WithProfile(ctx, runtimeTestScope(t), "profile-summary", profile)
			if err != nil {
				t.Fatal(err)
			}
			ledger := &accountingLedger{}
			factory.config.UsageLedger = ledger
			if _, err := store.Append(ctx, "profile-summary", session.AnySequence,
				session.PendingEvent{Type: session.EventModelRequested, Payload: session.MustJSONPayload(map[string]any{
					"request_id": "previous-request", "estimated_input_tokens": 30, "output_token_limit": 100,
				})},
				session.PendingEvent{Type: session.EventAssistantMessage, Payload: session.MustJSONPayload(map[string]any{
					"request_id": "previous-request", "prompt_tokens": 30, "completion_tokens": test.used,
				})},
			); err != nil {
				t.Fatal(err)
			}
			// Critical context permits fallback for ordinary provider failures,
			// but insufficient Profile quota must stop the complete operation.
			prepared, err := factory.PrepareMessages(ctx, "profile-summary", contextBudgetMessages(18))
			if !test.allow {
				var constraint *ProfileConstraintError
				if !errors.As(err, &constraint) || prepared != nil {
					t.Fatalf("insufficient output quota became shortened/fallback summary: prepared=%+v err=%v", prepared, err)
				}
				if len(fake.inputs) != 0 || len(ledger.requests) != 0 || len(ledger.records) != 0 {
					t.Fatalf("insufficient output quota reached model admission: provider=%d ledger=%+v", len(fake.inputs), ledger)
				}
			} else {
				if err != nil || len(prepared) != 4 || len(fake.inputs) != 1 {
					t.Fatalf("exact output quota rejected planned summary: prepared=%+v calls=%d err=%v", prepared, len(fake.inputs), err)
				}
				common := model.GetCommonOptions(nil, fake.options[0]...)
				if common.MaxTokens == nil || *common.MaxTokens != 100 || len(ledger.requests) != 1 || ledger.requests[0].OutputTokens != 100 {
					t.Fatalf("exact output quota changed planned allowance: options=%+v ledger=%+v", common, ledger)
				}
				if source := summaryFormatSource(strings.TrimPrefix(prepared[0].Content, "Conversation summary:\n")); source != summarySourceSemantic {
					t.Fatalf("exact output quota used fallback: source=%q", source)
				}
			}
			events, err := store.Load(ctx, "profile-summary", 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			compacted := 0
			for _, event := range events {
				if event.Type == session.EventContextCompacted {
					compacted++
				}
			}
			if (compacted == 1) != test.allow || compacted > 1 {
				t.Fatalf("unexpected durable compaction after quota check: allowed=%v compacted=%d", test.allow, compacted)
			}
		})
	}
}

type contextPolicyBudgetState struct {
	name     string
	snapshot tokenusage.BudgetSnapshot
	err      error
}

func contextPolicyBudgetStates() []contextPolicyBudgetState {
	return []contextPolicyBudgetState{
		{name: "low", snapshot: tokenusage.BudgetSnapshot{Limits: tokenusage.BudgetLimits{MaxTotalTokens: 1000, MaxCostMicros: 1000}, TotalTokens: 10, CostMicros: 10}},
		{name: "near token limit", snapshot: tokenusage.BudgetSnapshot{Limits: tokenusage.BudgetLimits{MaxTotalTokens: 1000}, TotalTokens: 850}},
		{name: "near cost limit", snapshot: tokenusage.BudgetSnapshot{Limits: tokenusage.BudgetLimits{MaxCostMicros: 1000}, CostMicros: 850}},
		{name: "reserved tokens", snapshot: tokenusage.BudgetSnapshot{Limits: tokenusage.BudgetLimits{MaxTotalTokens: 1000}, TotalTokens: 100, ReservedTotalTokens: 750}},
		{name: "reserved cost", snapshot: tokenusage.BudgetSnapshot{Limits: tokenusage.BudgetLimits{MaxCostMicros: 1000}, CostMicros: 100, ReservedCostMicros: 750}},
		{name: "exhausted", snapshot: tokenusage.BudgetSnapshot{Limits: tokenusage.BudgetLimits{MaxTotalTokens: 1000}, TotalTokens: 1000, Exhausted: true}},
		{name: "reader unavailable", err: errors.New("team budget storage unavailable")},
	}
}

type contextPolicyBudgetReader struct {
	snapshot tokenusage.BudgetSnapshot
	err      error
	calls    int
}

func (r *contextPolicyBudgetReader) Budget(context.Context, orchestration.TeamID) (tokenusage.BudgetSnapshot, error) {
	r.calls++
	return r.snapshot, r.err
}

func contextBudgetFactory(t *testing.T, store *memorystore.Store, fake model.BaseChatModel, reader *contextPolicyBudgetReader) *Factory {
	t.Helper()
	factory := profileFactory(t, store, fake)
	factory.config.ContextWindowTokens, factory.config.CompactionTriggerTokens = 600, 450
	factory.config.ActorScope.TeamID = "team-context-budget"
	factory.goalBudget, factory.config.GoalBudget = reader, reader
	return factory
}

func contextBudgetMessages(count int) []*schema.Message {
	messages := make([]*schema.Message, count)
	for index := range messages {
		content := fmt.Sprintf("original conversation fact %02d", index)
		if index%2 == 0 || index == count-1 {
			messages[index] = schema.UserMessage(content)
		} else {
			messages[index] = schema.AssistantMessage(content, nil)
		}
	}
	return messages
}

type refusingContextReservationLedger struct{ accountingLedger }

func (ledger *refusingContextReservationLedger) Reserve(_ context.Context, request tokenusage.ReservationRequest) (tokenusage.Reservation, error) {
	ledger.requests = append(ledger.requests, request)
	return tokenusage.Reservation{}, tokenusage.ErrBudgetExceeded
}
