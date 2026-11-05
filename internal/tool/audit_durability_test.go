package tool

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/policy"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/memorystore"
)

type cancelingAuditPolicy struct{ cancel context.CancelFunc }

func (engine cancelingAuditPolicy) Evaluate(context.Context, policy.Request) (policy.Decision, error) {
	engine.cancel()
	return policy.Decision{Effect: policy.EffectDeny, RuleID: "denied"}, nil
}

func TestProducedPolicyDecisionSurvivesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	registry := NewRegistry()
	if _, err := registry.Activate(testBinding("rev-1", "unused")); err != nil {
		t.Fatal(err)
	}
	events := memorystore.New()
	pipeline := mustPipeline(t, registry, cancelingAuditPolicy{cancel}, nil, events)
	_, _ = pipeline.Execute(ctx, Call{SessionID: "s", ToolName: "echo", Arguments: json.RawMessage(`{}`)})
	recorded, err := events.Load(context.Background(), "s", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range recorded {
		if event.Type == session.EventPolicyDecided {
			found = true
		}
	}
	if !found {
		t.Fatal("cancellation lost an already produced policy decision")
	}
}
