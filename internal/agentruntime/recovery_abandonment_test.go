package agentruntime

import (
	"context"
	"errors"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentgoal"
	"github.com/vkviyu/go-self-evolving-agent/internal/policy"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/memorystore"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

func TestAbandonedTurnCannotExecuteEvenWithPreconstructedRuntime(t *testing.T) {
	ctx := context.Background()
	events := memorystore.New()
	registry := tool.NewRegistry()
	engine, err := policy.DefaultDeny()
	if err != nil {
		t.Fatal(err)
	}
	pipeline, err := tool.NewPipeline(tool.PipelineConfig{Registry: registry, Policy: engine, Events: events})
	if err != nil {
		t.Fatal(err)
	}
	fake := &scriptedModel{}
	factory, err := NewFactory(Config{}, fake, registry, pipeline, events, events)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := factory.NewTurn(ctx, "session")
	if err != nil {
		t.Fatal(err)
	}
	// Even an opaque checkpoint survives abandonment; the gate must run first.
	if err := events.Set(ctx, checkpointID("session", turn.ID()), []byte("retained opaque evidence")); err != nil {
		t.Fatal(err)
	}
	if _, err := events.Append(ctx, "session", session.AnySequence,
		session.PendingEvent{Type: session.EventTurnStarted, CorrelationID: turn.ID(), Payload: []byte(`{}`)},
		session.PendingEvent{Type: session.EventTurnAbandoned, CorrelationID: turn.ID(), Payload: []byte(`{}`)},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := turn.Run(ctx, []*schema.Message{schema.UserMessage("must not execute")}, nil); !errors.Is(err, session.ErrRecoveryAbandoned) {
		t.Fatalf("preconstructed Run accepted: %v", err)
	}
	if _, err := turn.Resume(ctx, nil); !errors.Is(err, session.ErrRecoveryAbandoned) {
		t.Fatalf("preconstructed Resume accepted: %v", err)
	}
	if _, err := factory.ResumeTurn(ctx, "session", turn.ID()); !errors.Is(err, session.ErrRecoveryAbandoned) {
		t.Fatalf("factory resume accepted: %v", err)
	}
	if _, _, err := factory.CompletedTurn(ctx, "session", turn.ID()); !errors.Is(err, session.ErrRecoveryAbandoned) {
		t.Fatalf("completed result accepted: %v", err)
	}
	content := "expected"
	evaluation := agentgoal.Evaluation{Goal: agentgoal.Goal{SessionID: "session", Contract: &agentgoal.Contract{Objective: "verify", Scope: "workspace", Criteria: []agentgoal.Criterion{{Description: "file matches", Kind: "file_content", Path: "result.txt", Content: &content}}}}, Round: agentgoal.Round{TurnID: turn.ID()}}
	if _, err := (&fileGoalVerifier{factory: factory}).Verify(ctx, evaluation); !errors.Is(err, session.ErrRecoveryAbandoned) {
		t.Fatalf("verifier accepted abandoned task: %v", err)
	}
	records, err := factory.TurnRecords(ctx, "session")
	if err != nil || len(records) != 1 || !records[0].Abandoned || records[0].HasCheckpoint {
		t.Fatalf("records=%+v err=%v", records, err)
	}
	if fake.calls != 0 {
		t.Fatal("abandoned work contacted model")
	}
}
