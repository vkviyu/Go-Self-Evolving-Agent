package agentruntime

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/memorystore"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/sqlitestore"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenusage"
)

type interruptedUsageLedger struct {
	tokenusage.DurableReservationLedger
	failSettle                  error
	failDispatch                error
	commitDispatchBeforeFailure bool
}

func (ledger interruptedUsageLedger) MarkDispatched(ctx context.Context, id string) error {
	if ledger.failDispatch != nil && !ledger.commitDispatchBeforeFailure {
		return ledger.failDispatch
	}
	if err := ledger.DurableReservationLedger.MarkDispatched(ctx, id); err != nil {
		return err
	}
	return ledger.failDispatch
}

func (ledger interruptedUsageLedger) SettleReservation(ctx context.Context, id string, record tokenusage.Record) error {
	if ledger.failSettle != nil {
		return ledger.failSettle
	}
	return ledger.DurableReservationLedger.SettleReservation(ctx, id, record)
}

func openAccountingDB(t *testing.T, path string) (*sqlitestore.Database, orchestration.ActorScope) {
	t.Helper()
	database, err := sqlitestore.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	root, _, err := database.Orchestration().EnsureRootThread(context.Background(), orchestration.RootThreadSeed{SessionID: "usage"})
	if err != nil {
		t.Fatal(err)
	}
	return database, root.ActorScope()
}

func TestModelReceiptSurvivesCrashBeforeSettlement(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(map[bool]string{false: "generate", true: "stream"}[streaming], func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "accounting.db")
			database, scope := openAccountingDB(t, path)
			failure := errors.New("simulated interruption after receipt")
			ledger := interruptedUsageLedger{DurableReservationLedger: database.Orchestration(), failSettle: failure}
			message := schema.AssistantMessage("done", nil)
			message.ResponseMeta = &schema.ResponseMeta{Usage: &schema.TokenUsage{PromptTokens: 12, CompletionTokens: 3, TotalTokens: 15}}
			fake := usageModel{response: message}
			events := memorystore.New()
			factory := profileFactory(t, events, fake)
			factory.config.ActorScope, factory.config.UsageLedger = scope, ledger
			recorded := &recordingModel{factory: factory, inner: fake, events: events, sessionID: "usage", turnID: "turn"}
			var err error
			if streaming {
				var stream *schema.StreamReader[*schema.Message]
				stream, err = recorded.Stream(ctx, []*schema.Message{schema.UserMessage("hello")}, model.WithMaxTokens(7))
				if err == nil {
					err = drainModelStream(stream)
				}
			} else {
				_, err = recorded.Generate(ctx, []*schema.Message{schema.UserMessage("hello")}, model.WithMaxTokens(7))
			}
			if !errors.Is(err, failure) {
				t.Fatalf("lost settlement failure: %v", err)
			}
			budget, err := database.Orchestration().Budget(ctx, scope.TeamID)
			if err != nil || budget.ReservedTotalTokens != 37 || budget.TotalTokens != 0 {
				t.Fatalf("pending budget=%#v %v", budget, err)
			}
			if err := database.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, _ := openAccountingDB(t, path)
			if count, err := reopened.Orchestration().RecoverReservations(ctx); err != nil || count != 1 {
				t.Fatalf("recover=%d %v", count, err)
			}
			budget, err = reopened.Orchestration().Budget(ctx, scope.TeamID)
			if err != nil || budget.ReservedTotalTokens != 0 || budget.TotalTokens != 15 || budget.InputTokens != 12 || budget.OutputTokens != 3 {
				t.Fatalf("provider receipt lost=%#v %v", budget, err)
			}
		})
	}
}

func TestDispatchPersistenceFailureDoesNotCallProvider(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(map[bool]string{false: "uncommitted", true: "uncertain-commit"}[committed], func(t *testing.T) {
			ctx := context.Background()
			database, scope := openAccountingDB(t, ":memory:")
			failure := errors.New("dispatch persistence failed")
			ledger := interruptedUsageLedger{DurableReservationLedger: database.Orchestration(), failDispatch: failure, commitDispatchBeforeFailure: committed}
			fake, events := &inspectingProfileModel{}, memorystore.New()
			factory := profileFactory(t, events, fake)
			factory.config.ActorScope, factory.config.UsageLedger = scope, ledger
			recorded := &recordingModel{factory: factory, inner: fake, events: events, sessionID: "usage", turnID: "turn"}
			_, err := recorded.Generate(ctx, []*schema.Message{schema.UserMessage("hello")}, model.WithMaxTokens(7))
			if !errors.Is(err, failure) || len(fake.input) != 0 {
				t.Fatalf("provider called after failure: %v %v", fake.input, err)
			}
			if _, err := database.Orchestration().RecoverReservations(ctx); err != nil {
				t.Fatal(err)
			}
			budget, err := database.Orchestration().Budget(ctx, scope.TeamID)
			want := 0
			if committed {
				want = 37
			}
			if err != nil || budget.TotalTokens != want || budget.ReservedTotalTokens != 0 {
				t.Fatalf("uncertain dispatch refunded=%#v %v", budget, err)
			}
		})
	}
}

func TestModelRequestSequenceIncludesReservationsWithoutEvents(t *testing.T) {
	ctx := context.Background()
	database, scope := openAccountingDB(t, ":memory:")
	ledger := database.Orchestration()
	if _, err := ledger.Reserve(ctx, tokenusage.ReservationRequest{TeamID: scope.TeamID, AgentID: scope.AgentID, TurnID: "turn", RequestID: "turn:7", InputTokens: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.RecoverReservations(ctx); err != nil {
		t.Fatal(err)
	}
	events, fake := memorystore.New(), usageModel{response: schema.AssistantMessage("done", nil)}
	factory := profileFactory(t, events, fake)
	factory.config.ActorScope, factory.config.UsageLedger = scope, ledger
	sequence, err := factory.modelRequestSequence(ctx, "usage", "turn")
	if err != nil || sequence != 7 {
		t.Fatalf("sequence=%d %v", sequence, err)
	}
	recorded := &recordingModel{factory: factory, inner: fake, events: events, sessionID: "usage", turnID: "turn"}
	recorded.requestSequence.Store(sequence)
	if _, err := recorded.Generate(ctx, []*schema.Message{schema.UserMessage("hello")}); err != nil {
		t.Fatal(err)
	}
	if highest, err := ledger.ModelRequestSequence(ctx, scope.TeamID, "turn"); err != nil || highest != 8 {
		t.Fatalf("reused reserved identity=%d %v", highest, err)
	}
}

func TestModelReservationIncludesConfiguredPrice(t *testing.T) {
	events, ledger := memorystore.New(), &accountingLedger{}
	fake := usageModel{response: schema.AssistantMessage("done", nil)}
	factory := profileFactory(t, events, fake)
	factory.config.UsageLedger = ledger
	factory.config.Pricing = tokenusage.NewPriceTable(map[string]tokenusage.Price{"test/parent": {InputMicrosPerMillion: 1_000_000, OutputMicrosPerMillion: 2_000_000}})
	recorded := &recordingModel{factory: factory, inner: fake, events: events, sessionID: "usage", turnID: "turn"}
	if _, err := recorded.Generate(context.Background(), []*schema.Message{schema.UserMessage("hello")}, model.WithMaxTokens(7)); err != nil {
		t.Fatal(err)
	}
	if len(ledger.requests) != 1 || ledger.requests[0].CostMicros != 44 {
		t.Fatalf("unpriced reservation=%#v", ledger.requests)
	}
}
