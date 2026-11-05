package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/policy"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/memorystore"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenizer"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

// This model exercises the real Eino loop: two confirmed tool decisions,
// compaction between them, then an unavailable provider before the final answer.
type journalRecoveryModel struct {
	calls, summaries int
	inputs           [][]*schema.Message
}

type journalOutcomeModel struct {
	calls  int
	inputs [][]*schema.Message
}

func (m *journalOutcomeModel) Generate(_ context.Context, input []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	m.calls++
	m.inputs = append(m.inputs, input)
	if m.calls == 1 {
		return schema.AssistantMessage("", []schema.ToolCall{{ID: "outcome-call", Type: "function", Function: schema.FunctionCall{Name: "echo", Arguments: `{}`}}}), nil
	}
	if m.calls == 2 {
		return nil, errors.New("provider temporarily unavailable")
	}
	return schema.AssistantMessage("continued with original tool error", nil), nil
}

func (m *journalOutcomeModel) Stream(ctx context.Context, input []*schema.Message, options ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	message, err := m.Generate(ctx, input, options...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{message}), nil
}

type journalOutcomePolicy struct {
	effect policy.Effect
	calls  int
}

func (engine *journalOutcomePolicy) Evaluate(context.Context, policy.Request) (policy.Decision, error) {
	engine.calls++
	return policy.Decision{Effect: engine.effect, Reason: "original host policy"}, nil
}

type journalOutcomeApprover struct{ calls int }

func (approver *journalOutcomeApprover) Approve(context.Context, policy.Request, policy.Decision) (bool, error) {
	approver.calls++
	return false, nil
}

func TestJournalRecoveryPreservesModelVisibleToolFailures(t *testing.T) {
	for _, kind := range []string{"execution", "policy_denied", "approval_denied", "precondition"} {
		t.Run(kind, func(t *testing.T) {
			ctx, store, registry := context.Background(), memorystore.New(), tool.NewRegistry()
			invocations := 0
			binding := profileBinding("echo", tool.RiskWrite, func(context.Context, json.RawMessage) (json.RawMessage, error) {
				invocations++
				if kind == "precondition" {
					return nil, &tool.PreconditionError{Code: "missing_evidence", Message: "inspect first"}
				}
				return nil, errors.New("known executor failure")
			})
			if _, err := registry.Activate(binding); err != nil {
				t.Fatal(err)
			}
			engine, approver := &journalOutcomePolicy{effect: policy.EffectAllow}, &journalOutcomeApprover{}
			if kind == "policy_denied" {
				engine.effect = policy.EffectDeny
			} else if kind == "approval_denied" {
				engine.effect = policy.EffectRequireApproval
			}
			pipeline, err := tool.NewPipeline(tool.PipelineConfig{Registry: registry, Policy: engine, Approver: approver, Events: store})
			if err != nil {
				t.Fatal(err)
			}
			fake := &journalOutcomeModel{}
			factory, err := NewFactory(Config{Instruction: "Preserve tool outcomes."}, fake, registry, pipeline, store, store)
			if err != nil {
				t.Fatal(err)
			}
			turn, err := factory.NewTurn(ctx, session.ID("outcome-"+kind))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := turn.Run(ctx, []*schema.Message{schema.UserMessage("try echo")}, nil); err == nil {
				t.Fatal("provider interruption did not stop the turn")
			}
			resumed, err := factory.ResumeTurn(ctx, session.ID("outcome-"+kind), turn.ID())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := resumed.Resume(ctx, nil); err != nil {
				t.Fatal(err)
			}
			if fake.calls != 3 || invocations > 1 || engine.calls != 1 || approver.calls > 1 || rawRequestDigest(fake.inputs[1]) != rawRequestDigest(fake.inputs[2]) {
				t.Fatalf("recovery changed the observed outcome or repeated work: model=%d tools=%d policies=%d approvals=%d", fake.calls, invocations, engine.calls, approver.calls)
			}
		})
	}
}

func TestTurnFailureClassifiesOnlyUncancelledIterationExhaustion(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		if canceled {
			cancel()
		}
		turn := &Turn{events: memorystore.New(), sessionID: "iteration", turnID: "turn"}
		err := turn.fail(ctx, adk.ErrExceedMaxIterations)
		cancel()
		_, classified := errors.AsType[*orchestration.TaskAttemptFailure](err)
		if classified == canceled || !errors.Is(err, adk.ErrExceedMaxIterations) {
			t.Fatalf("canceled=%v classified=%v err=%v", canceled, classified, err)
		}
	}
}

func (m *journalRecoveryModel) Generate(_ context.Context, input []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	if len(input) > 0 && strings.HasPrefix(input[0].Content, compactionInstruction) {
		m.summaries++
		return schema.AssistantMessage(testSemanticSummary, nil), nil
	}
	m.calls++
	m.inputs = append(m.inputs, input)
	if m.calls <= 2 {
		return schema.AssistantMessage("", []schema.ToolCall{{ID: []string{"journal-read-1", "journal-read-2"}[m.calls-1], Type: "function", Function: schema.FunctionCall{Name: "read", Arguments: `{}`}}}), nil
	}
	if m.calls == 3 {
		return nil, errors.New("provider temporarily unavailable")
	}
	return schema.AssistantMessage("recovered after confirmed work", nil), nil
}

func (m *journalRecoveryModel) Stream(ctx context.Context, input []*schema.Message, options ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	message, err := m.Generate(ctx, input, options...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{message}), nil
}

func TestJournalRecoveryReplaysEinoDecisionsWithoutRepeatingPreparation(t *testing.T) {
	ctx, store, fake := context.Background(), memorystore.New(), &journalRecoveryModel{}
	toolCalls := 0
	factory := profileFactory(t, store, fake, profileBinding("read", tool.RiskRead, func(context.Context, json.RawMessage) (json.RawMessage, error) {
		toolCalls++
		if toolCalls == 1 {
			return json.Marshal(map[string]string{"content": strings.Repeat("read output ", 450)})
		}
		return json.RawMessage(`{"content":"small"}`), nil
	}))
	factory.config.Instruction = "Stable recovery instruction."
	factory.config.ContextWindowTokens, factory.config.CompactionTriggerTokens, factory.config.MaxOutputTokens = 8000, 1800, 256
	factory.tokenizer = tokenizer.NewApproximate("test")
	mailbox := &fakeBackgroundMailbox{queued: []orchestration.Message{backgroundMessage("original")}}
	factory = factory.WithMailbox(mailbox)
	turn, err := factory.NewTurn(ctx, "journal-loop")
	if err != nil {
		t.Fatal(err)
	}
	_, err = turn.Run(ctx, []*schema.Message{
		schema.UserMessage(strings.Repeat("old question ", 130)),
		schema.AssistantMessage(strings.Repeat("old findings ", 130), nil),
		schema.UserMessage("Inspect the file and explain it; do not modify anything."),
	}, nil)
	if err == nil || fake.calls != 3 || toolCalls != 2 || fake.summaries != 1 {
		t.Fatalf("unexpected interrupted boundary: calls=%d tools=%d summaries=%d err=%v", fake.calls, toolCalls, fake.summaries, err)
	}
	claimsBefore := mailbox.claims
	mailbox.queued = []orchestration.Message{backgroundMessage("arrived-after-outage")}
	if _, found, err := store.Get(ctx, checkpointID("journal-loop", turn.ID())); err != nil || found {
		t.Fatalf("test requires journal recovery without an Eino interrupt checkpoint: found=%v err=%v", found, err)
	}
	resumed, err := factory.ResumeTurn(ctx, "journal-loop", turn.ID())
	if err != nil {
		t.Fatal(err)
	}
	result, err := resumed.Resume(ctx, nil)
	if err != nil || result.Final == nil || result.Final.Content != "recovered after confirmed work" {
		t.Fatalf("journal recovery failed: result=%+v err=%v", result, err)
	}
	if toolCalls != 2 || fake.calls != 4 || fake.summaries != 1 || mailbox.claims != claimsBefore || len(mailbox.queued) != 1 {
		t.Fatalf("replay repeated work: tools=%d model=%d summaries=%d claims=%d->%d queued=%d", toolCalls, fake.calls, fake.summaries, claimsBefore, mailbox.claims, len(mailbox.queued))
	}
	for index, input := range fake.inputs {
		encoded, _ := json.Marshal(input)
		if strings.Count(string(encoded), factory.config.Instruction) != 1 || strings.Contains(string(encoded), "arrived-after-outage") {
			t.Fatalf("request %d duplicated its instruction or changed input during reconstruction: %s", index, encoded)
		}
	}
	if rawRequestDigest(fake.inputs[2]) != rawRequestDigest(fake.inputs[3]) {
		t.Fatal("the replacement request did not retain the prepared compacted view")
	}
	events, err := store.Load(ctx, "journal-loop", 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	responses := 0
	for _, event := range events {
		if event.Type == session.EventAssistantMessage && event.CorrelationID == turn.ID() {
			responses++
		}
	}
	if responses != 3 {
		t.Fatalf("replayed decisions were persisted as new model answers: %d", responses)
	}
}
