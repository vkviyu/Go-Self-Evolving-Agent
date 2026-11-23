package agentruntime

import (
	"context"
	"errors"
	"io"
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

func TestRecordingModelEmitsProviderLifecycleEvents(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(map[bool]string{false: "generate", true: "stream"}[streaming], func(t *testing.T) {
			store := memorystore.New()
			var events []Event
			recorded := &recordingModel{
				inner:             lifecycleModel{},
				events:            store,
				sessionID:         session.ID("lifecycle"),
				turnID:            "turn-1",
				runtimeSnapshotID: "registry-1",
				liveSink: func(_ context.Context, event Event) error {
					events = append(events, event)
					return nil
				},
			}
			input := []*schema.Message{schema.UserMessage("hello")}
			if streaming {
				stream, err := recorded.Stream(context.Background(), input)
				if err != nil {
					t.Fatal(err)
				}
				for {
					_, recvErr := stream.Recv()
					if recvErr != nil {
						if !errors.Is(recvErr, io.EOF) {
							t.Fatal(recvErr)
						}
						break
					}
				}
				stream.Close()
			} else if _, err := recorded.Generate(context.Background(), input); err != nil {
				t.Fatal(err)
			}
			want := []EventType{
				EventModelPreparationStarted,
				EventTurnPreparedPersistenceStarted,
				EventModelBudgetReservationStarted,
				EventModelRequestPersistenceStarted,
				EventModelRequestStarted,
				EventModelResponseStarted,
			}
			if len(events) != len(want) {
				t.Fatalf("lifecycle events = %#v, want %v", events, want)
			}
			for i, event := range events {
				if event.Type != want[i] || event.TurnID != "turn-1" || event.Purpose != "" {
					t.Fatalf("lifecycle event %d = %#v, want %s for turn-1", i, event, want[i])
				}
			}
		})
	}
}

type requestStageStore struct {
	session.EventStore
	beforeAppend func(session.PendingEvent) error
}

func (s requestStageStore) Append(ctx context.Context, id session.ID, expected uint64, events ...session.PendingEvent) ([]session.Event, error) {
	for _, event := range events {
		if err := s.beforeAppend(event); err != nil {
			return nil, err
		}
	}
	return s.EventStore.Append(ctx, id, expected, events...)
}

func TestRecordingModelEmitsPersistenceStageBeforeJournalAppend(t *testing.T) {
	for _, boundary := range []struct {
		durable session.EventType
		live    EventType
	}{
		{session.EventTurnPrepared, EventTurnPreparedPersistenceStarted},
		{session.EventModelRequested, EventModelRequestPersistenceStarted},
	} {
		for _, streaming := range []bool{false, true} {
			t.Run(string(boundary.durable)+map[bool]string{false: "/generate", true: "/stream"}[streaming], func(t *testing.T) {
				var latest Event
				called := false
				failure := errors.New("journal unavailable")
				store := requestStageStore{
					EventStore: memorystore.New(),
					beforeAppend: func(event session.PendingEvent) error {
						if event.Type != boundary.durable {
							return nil
						}
						called = true
						if latest.Type != boundary.live || latest.TurnID != "turn" {
							t.Fatalf("stage on entering Append(%s) = %#v", event.Type, latest)
						}
						return failure
					},
				}
				provider := &inspectingProfileModel{}
				recorded := &recordingModel{
					inner: provider, events: store, sessionID: "journal-stage", turnID: "turn",
					liveSink: func(_ context.Context, event Event) error {
						latest = event
						return nil
					},
				}
				input := []*schema.Message{schema.UserMessage("hello")}
				var err error
				if streaming {
					_, err = recorded.Stream(t.Context(), input)
				} else {
					_, err = recorded.Generate(t.Context(), input)
				}
				if !called || !errors.Is(err, failure) || len(provider.input) != 0 {
					t.Fatalf("persistence boundary: called=%v err=%v provider input=%v", called, err, provider.input)
				}
				if latest.Type != boundary.live {
					t.Fatalf("stage advanced after failed persistence: %#v", latest)
				}
			})
		}
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

type lifecycleModel struct{}

var _ model.BaseChatModel = (lifecycleModel{})

func (lifecycleModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	return schema.AssistantMessage("done", nil), nil
}

func (lifecycleModel) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return schema.StreamReaderFromArray([]*schema.Message{schema.AssistantMessage("done", nil)}), nil
}
