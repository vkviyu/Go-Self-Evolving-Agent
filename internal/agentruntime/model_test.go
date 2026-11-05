package agentruntime

import (
	"context"
	"errors"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/memorystore"
)

func TestRecordingModelReturnsNonNilErrorStream(t *testing.T) {
	store := memorystore.New()
	model := &recordingModel{
		inner:             failingStreamModel{},
		events:            store,
		sessionID:         session.ID("stream-failure"),
		turnID:            "turn-1",
		runtimeSnapshotID: "registry-1",
	}
	stream, err := model.Stream(context.Background(), []*schema.Message{schema.UserMessage("hello")})
	if err != nil {
		t.Fatalf("Stream returned setup error: %v", err)
	}
	if stream == nil {
		t.Fatal("Stream returned a nil reader")
	}
	_, err = stream.Recv()
	stream.Close()
	if err == nil || !errors.Is(err, errSyntheticStream) {
		t.Fatalf("stream error = %v", err)
	}
}

var errSyntheticStream = errors.New("synthetic stream failure")

type failingStreamModel struct{}

var _ model.BaseChatModel = (failingStreamModel{})

func (failingStreamModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	return nil, errSyntheticStream
}

func (failingStreamModel) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return nil, errSyntheticStream
}
