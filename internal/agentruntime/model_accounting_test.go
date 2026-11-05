package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/memorystore"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenusage"
)

type accountingLedger struct {
	requests        []tokenusage.ReservationRequest
	records         []tokenusage.Record
	settlementCalls int
	released        int
	failSettle      error
}

func (ledger *accountingLedger) CanAdmit(context.Context, orchestration.TeamID) (bool, error) {
	return true, nil
}
func (ledger *accountingLedger) RecordUsage(_ context.Context, record tokenusage.Record) error {
	ledger.records = append(ledger.records, record)
	return nil
}
func (ledger *accountingLedger) Reserve(_ context.Context, request tokenusage.ReservationRequest) (tokenusage.Reservation, error) {
	ledger.requests = append(ledger.requests, request)
	return tokenusage.Reservation{ID: request.RequestID}, nil
}
func (ledger *accountingLedger) SettleReservation(ctx context.Context, _ string, record tokenusage.Record) error {
	ledger.settlementCalls++
	if ledger.failSettle != nil {
		return ledger.failSettle
	}
	return ledger.RecordUsage(ctx, record)
}
func (ledger *accountingLedger) ReleaseReservation(context.Context, string, string) error {
	ledger.released++
	return nil
}

type usageModel struct {
	response *schema.Message
	fail     error
}

func (fake usageModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	return fake.response, fake.fail
}
func (fake usageModel) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	if fake.fail != nil {
		return nil, fake.fail
	}
	return schema.StreamReaderFromArray([]*schema.Message{fake.response}), nil
}

func TestRecordingModelChargesReportedOrReservedUsageIncludingFailures(t *testing.T) {
	usage := schema.AssistantMessage("done", nil)
	usage.ResponseMeta = &schema.ResponseMeta{Usage: &schema.TokenUsage{PromptTokens: 12, CompletionTokens: 3, TotalTokens: 15}}
	for _, scenario := range []struct {
		name                  string
		fake                  usageModel
		wantInput, wantOutput int
		wantQuality           string
	}{
		{"reported", usageModel{response: usage}, 12, 3, "provider"},
		{"missing", usageModel{response: schema.AssistantMessage("done", nil)}, 30, 7, "estimated_reservation"},
		{"failed", usageModel{fail: errSyntheticStream}, 30, 7, "estimated_reservation"},
	} {
		for _, streaming := range []bool{false, true} {
			t.Run(scenario.name+map[bool]string{false: "/generate", true: "/stream"}[streaming], func(t *testing.T) {
				store, ledger := memorystore.New(), &accountingLedger{}
				factory := profileFactory(t, store, scenario.fake)
				factory.config.UsageLedger = ledger
				recorded := &recordingModel{factory: factory, inner: scenario.fake, events: store, sessionID: "usage", turnID: "turn"}
				var err error
				if streaming {
					var stream *schema.StreamReader[*schema.Message]
					stream, err = recorded.Stream(context.Background(), []*schema.Message{schema.UserMessage("hello")}, model.WithMaxTokens(7))
					if err == nil {
						err = drainModelStream(stream)
					}
				} else {
					_, err = recorded.Generate(context.Background(), []*schema.Message{schema.UserMessage("hello")}, model.WithMaxTokens(7))
				}
				if !errors.Is(err, scenario.fake.fail) {
					t.Fatalf("model error: %v", err)
				}
				if len(ledger.requests) != 1 || ledger.requests[0].OutputTokens != 7 || ledger.released != 0 || len(ledger.records) != 1 {
					t.Fatalf("accounting=%#v", ledger)
				}
				record := ledger.records[0]
				if record.Usage.Input != scenario.wantInput || record.Usage.Output != scenario.wantOutput || record.Quality != scenario.wantQuality {
					t.Fatalf("usage=%#v", record)
				}
			})
		}
	}
}

func TestStreamSettlementFailureDoesNotDowngradeKnownProviderUsage(t *testing.T) {
	store := memorystore.New()
	ledger := &accountingLedger{failSettle: errors.New("ledger unavailable")}
	message := schema.AssistantMessage("done", nil)
	message.ResponseMeta = &schema.ResponseMeta{Usage: &schema.TokenUsage{PromptTokens: 12, CompletionTokens: 3, TotalTokens: 15}}
	fake := usageModel{response: message}
	factory := profileFactory(t, store, fake)
	factory.config.UsageLedger = ledger
	recorded := &recordingModel{factory: factory, inner: fake, events: store, sessionID: "usage", turnID: "turn"}
	stream, err := recorded.Stream(context.Background(), []*schema.Message{schema.UserMessage("hello")})
	if err != nil {
		t.Fatal(err)
	}
	if err := drainModelStream(stream); !errors.Is(err, ledger.failSettle) {
		t.Fatalf("lost settlement error: %v", err)
	}
	if ledger.settlementCalls != 1 || ledger.released != 0 {
		t.Fatalf("known usage was retried as an estimate: %#v", ledger)
	}
	if _, pending := recorded.reservations.Load("turn:1"); !pending {
		t.Fatal("unsettled reservation discarded")
	}
}

type rejectModelRequests struct{ *memorystore.Store }

func (store rejectModelRequests) Append(ctx context.Context, id session.ID, sequence uint64, events ...session.PendingEvent) ([]session.Event, error) {
	for _, event := range events {
		if event.Type == session.EventModelRequested {
			return nil, errors.New("request log unavailable")
		}
	}
	return store.Store.Append(ctx, id, sequence, events...)
}

func TestUndispatchedRequestRefundsReservationWithoutCallingProvider(t *testing.T) {
	store, ledger, fake := memorystore.New(), &accountingLedger{}, &inspectingProfileModel{}
	factory := profileFactory(t, store, fake)
	factory.config.UsageLedger = ledger
	recorded := &recordingModel{factory: factory, inner: fake, events: rejectModelRequests{store}, sessionID: "usage", turnID: "turn"}
	_, err := recorded.Generate(context.Background(), []*schema.Message{schema.UserMessage("hello")})
	if err == nil || len(fake.input) != 0 || ledger.released != 1 || len(ledger.records) != 0 {
		t.Fatalf("unsent accounting: err=%v provider_called=%v ledger=%#v", err, len(fake.input) != 0, ledger)
	}
}

func TestProfileReconcilesProviderUsageBeforeNextOutputAllowance(t *testing.T) {
	ctx := context.Background()
	store := memorystore.New()
	response := schema.AssistantMessage("done", nil)
	response.ResponseMeta = &schema.ResponseMeta{Usage: &schema.TokenUsage{PromptTokens: 12, CompletionTokens: 3, TotalTokens: 15}}
	fake := usageModel{response: response}
	parent := profileFactory(t, store, fake)
	profile, scope := runtimeTestProfile(), runtimeTestScope(t)
	profile.Limits.MaxOutputTokens = 7
	child, err := parent.WithProfile(ctx, scope, "child", profile)
	if err != nil {
		t.Fatal(err)
	}
	recorded := &recordingModel{factory: child, inner: fake, events: store, sessionID: "child", turnID: "turn"}
	input := []*schema.Message{schema.UserMessage("hello")}
	if _, err := recorded.Generate(ctx, input); err != nil {
		t.Fatal(err)
	}
	options, err := recorded.profileOptions(ctx, input, nil)
	if err != nil {
		t.Fatal(err)
	}
	common := model.GetCommonOptions(nil, options...)
	if common.MaxTokens == nil || *common.MaxTokens != 4 {
		t.Fatalf("remaining allowance=%v", common.MaxTokens)
	}
}

func TestProfileTokenBudgetsSurviveFactoryRecreation(t *testing.T) {
	ctx := context.Background()
	for _, kind := range []string{"input", "output"} {
		t.Run(kind, func(t *testing.T) {
			store := memorystore.New()
			fake := &inspectingProfileModel{}
			parent := profileFactory(t, store, fake)
			profile, scope := runtimeTestProfile(), runtimeTestScope(t)
			if kind == "input" {
				profile.Limits.MaxInputTokens = 30
			} else {
				profile.Limits.MaxOutputTokens = 7
			}
			for attempt := range 2 {
				child, err := parent.WithProfile(ctx, scope, "child", profile)
				if err != nil {
					t.Fatal(err)
				}
				recorded := &recordingModel{factory: child, inner: fake, events: store, sessionID: "child", turnID: "turn"}
				_, err = recorded.Generate(ctx, []*schema.Message{schema.UserMessage("hello")})
				var constraint *ProfileConstraintError
				if attempt == 0 && err != nil {
					t.Fatal(err)
				}
				if attempt == 1 && !errors.As(err, &constraint) {
					t.Fatalf("%s budget reset: %v", kind, err)
				}
			}
		})
	}
}

func TestCompactionAndRepeatedAssessmentUseSameLedgerWithUniqueRequests(t *testing.T) {
	ctx := context.Background()
	store, ledger := memorystore.New(), &accountingLedger{}
	factory := profileFactory(t, store, &summaryModel{})
	factory.config.UsageLedger = ledger
	for range 2 {
		if _, err := factory.summarize(ctx, "helpers", []*schema.Message{schema.UserMessage("summarize me")}, ContextHints{}); err != nil {
			t.Fatal(err)
		}
	}
	factory.model = &goalEvaluatorModel{responses: []string{`{"status":"complete","reason":"verified"}`, `{"status":"complete","reason":"verified"}`}}
	evaluation := goalEvaluationFixture()
	evaluation.Goal.SessionID = "helpers"
	for range 2 {
		evaluator := &modelGoalEvaluator{factory: factory}
		if _, err := evaluator.Evaluate(ctx, evaluation); err != nil {
			t.Fatal(err)
		}
	}
	if len(ledger.records) != 4 {
		t.Fatalf("helper calls not accounted: %d", len(ledger.records))
	}
	seen := map[string]bool{}
	for _, record := range ledger.records {
		if seen[record.RequestID] || record.Usage.Total == 0 {
			t.Fatalf("duplicate/free helper call: %#v", record)
		}
		seen[record.RequestID] = true
	}
	events, err := store.Load(ctx, "helpers", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	purposes := map[string]int{}
	for _, event := range events {
		if event.Type != session.EventModelRequested {
			continue
		}
		var payload struct {
			Purpose string `json:"purpose"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		purposes[payload.Purpose]++
	}
	if purposes["compaction"] != 2 || purposes["goal_assessment"] != 2 {
		t.Fatalf("helper purposes=%v", purposes)
	}
	history, err := factory.LoadHistory(ctx, "helpers")
	if err != nil || len(history) != 0 {
		t.Fatalf("helper messages leaked into conversation: %v %v", history, err)
	}
	for _, record := range ledger.records {
		if !strings.Contains(record.RequestID, ":") {
			t.Fatal("invalid request identity")
		}
	}
}
