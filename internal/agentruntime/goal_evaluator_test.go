package agentruntime

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentgoal"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/memorystore"
)

func TestModelGoalEvaluatorRetriesMalformedOutput(t *testing.T) {
	t.Parallel()
	model := &goalEvaluatorModel{responses: []string{"not json", `{"status":"complete","reason":"verified"}`}}
	store := memorystore.New()
	evaluator := &modelGoalEvaluator{factory: &Factory{model: model, events: store, config: Config{MaxOutputTokens: 512}}}
	verdict, err := evaluator.Evaluate(context.Background(), goalEvaluationFixture())
	if err != nil {
		t.Fatal(err)
	}
	if verdict.Decision != agentgoal.DecisionComplete || model.callCount() != 2 {
		t.Fatalf("verdict=%#v calls=%d", verdict, model.callCount())
	}
}

func TestModelGoalEvaluatorFailsAfterMalformedRetryWithoutRepeatingTask(t *testing.T) {
	t.Parallel()
	model := &goalEvaluatorModel{responses: []string{"bad", "still bad"}}
	store := memorystore.New()
	evaluator := &modelGoalEvaluator{factory: &Factory{model: model, events: store, config: Config{MaxOutputTokens: 512}}}
	verdict, err := evaluator.Evaluate(context.Background(), goalEvaluationFixture())
	if err == nil || verdict.Decision != "" || model.callCount() != 2 {
		t.Fatalf("verdict=%#v calls=%d", verdict, model.callCount())
	}
}

func goalEvaluationFixture() agentgoal.Evaluation {
	return agentgoal.Evaluation{
		Goal:  agentgoal.Goal{ID: "goal-test", SessionID: "goal-evaluator-session", Text: "finish the task"},
		Round: agentgoal.Round{Number: 1}, Candidate: "candidate",
	}
}

type goalEvaluatorModel struct {
	mu        sync.Mutex
	responses []string
	calls     int
}

func (fake *goalEvaluatorModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.calls >= len(fake.responses) {
		return nil, errors.New("unexpected evaluator call")
	}
	response := schema.AssistantMessage(fake.responses[fake.calls], nil)
	fake.calls++
	return response, nil
}

func (fake *goalEvaluatorModel) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return nil, errors.New("unexpected evaluator stream")
}

func (fake *goalEvaluatorModel) callCount() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.calls
}
