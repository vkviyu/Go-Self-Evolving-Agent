package conversation

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/policy"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/memorystore"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

func TestGreetingTextReachesUIBeforeModelStreamCompletes(t *testing.T) {
	chatModel := &greetingStreamModel{release: make(chan struct{})}
	store := memorystore.New()
	registry := tool.NewRegistry()
	engine, err := policy.DefaultDeny()
	if err != nil {
		t.Fatal(err)
	}
	pipeline, err := tool.NewPipeline(tool.PipelineConfig{Registry: registry, Policy: engine, Events: store})
	if err != nil {
		t.Fatal(err)
	}
	factory, err := agentruntime.NewFactory(agentruntime.Config{}, chatModel, registry, pipeline, store, store)
	if err != nil {
		t.Fatal(err)
	}
	conversation := &Conversation{factory: factory, sessionID: "stream"}
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
	if visible != "你好！" {
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
