package conversation

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentgoal"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
)

func TestGreetingTextReachesUIBeforeModelStreamCompletes(t *testing.T) {
	chatModel := &greetingStreamModel{release: make(chan struct{})}
	conversation := newGoalConversation(t, chatModel, agentgoal.Config{Enabled: true, Assessment: agentgoal.AssessmentHybrid}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var once sync.Once
	var visible string
	result, err := conversation.RunTurn(ctx, "你好", func(_ context.Context, event agentruntime.Event) error {
		if event.Type == agentruntime.EventAssistantTextDelta {
			visible += event.Delta
			once.Do(func() { close(chatModel.release) })
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if visible != "你好！" || result.GoalID != "" {
		t.Fatalf("visible=%q result=%+v", visible, result)
	}
}

type greetingStreamModel struct {
	release chan struct{}
}

func (*greetingStreamModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	return nil, errors.New("greeting must not invoke an independent evaluator")
}

func (fake *greetingStreamModel) Stream(ctx context.Context, _ []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	reader, writer := schema.Pipe[*schema.Message](1)
	go func() {
		defer writer.Close()
		if writer.Send(schema.AssistantMessage("你", nil), nil) {
			return
		}
		select {
		case <-fake.release:
			writer.Send(schema.AssistantMessage("好！", nil), nil)
		case <-ctx.Done():
			writer.Send(nil, ctx.Err())
		}
	}()
	return reader, nil
}
