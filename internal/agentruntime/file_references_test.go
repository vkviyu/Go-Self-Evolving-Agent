package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/modelcontext"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/memorystore"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenizer"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

func TestFileReferencesInOpeningRequestAndRecovery(t *testing.T) {
	for _, mode := range []string{"ordinary", "pending opening request", "confirmed opening request"} {
		t.Run(mode, func(t *testing.T) {
			ctx, store := context.Background(), memorystore.New()
			fake := &fileReferenceTestModel{failFirst: mode == "pending opening request"}
			factory := profileFactory(t, store, fake, profileBinding("workspace_read", tool.RiskRead, func(context.Context, json.RawMessage) (json.RawMessage, error) {
				return json.RawMessage(`{"content":"body"}`), nil
			}))
			factory.tokenizer = tokenizer.NewApproximate("test")
			const query = "Read @docs/design.md"
			inspections := 0
			factory.config.InspectFileReferences = func(_ context.Context, pipeline *tool.Pipeline, call tool.Call, got string) ([]FileReference, error) {
				inspections++
				if got != query || pipeline != factory.pipeline || call.Registry == nil || call.ToolName != "workspace_read" {
					t.Errorf("unexpected inspection: query=%q call=%+v", got, call)
				}
				return []FileReference{{Path: "docs/design.md", SizeBytes: 123, Headings: []FileHeading{{Line: 9, Level: 2, Title: "Execution"}}}}, nil
			}
			if mode == "confirmed opening request" {
				// Stop after Eino receives the confirmed first response but before
				// the next prompt can be stored or sent. Recovery must rebuild it.
				factory.events = &fileReferenceJournalFailure{EventStore: store}
			}
			const id session.ID = "file-reference-turn"
			turn, err := factory.NewTurn(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			result, err := turn.Run(ctx, []*schema.Message{schema.UserMessage(query)}, nil)
			if mode != "ordinary" {
				if err == nil {
					t.Fatal("expected interrupted turn")
				}
				resumed, err := factory.ResumeTurn(ctx, id, turn.ID())
				if err != nil {
					t.Fatal(err)
				}
				result, err = resumed.Resume(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if result.Final == nil || result.Final.Content != "finished" || inspections != 1 || len(fake.inputs) < 2 {
				t.Fatalf("unexpected turn: result=%+v inspections=%d dispatches=%d", result, inspections, len(fake.inputs))
			}
			opening := fake.inputs[0][len(fake.inputs[0])-1]
			for _, part := range []string{query + fileReferenceHeader, `"size_bytes":123`, `"line":9`, `"estimated_remaining_input_tokens_before_metadata":`} {
				if opening.Role != schema.User || !strings.Contains(opening.Content, part) {
					t.Errorf("opening prompt missing %q: %s", part, opening.Content)
				}
			}
			if mode == "pending opening request" && (len(fake.inputs) != 3 || rawRequestDigest(fake.inputs[0]) != rawRequestDigest(fake.inputs[1])) {
				t.Fatalf("opening request changed when replaying the prepared prompt: %d dispatches", len(fake.inputs))
			}
			last := fake.inputs[len(fake.inputs)-1]
			for _, message := range last {
				if strings.Contains(message.Content, fileReferenceHeader) {
					t.Fatalf("later model call retained opening metadata: %s", message.Content)
				}
			}
			history, err := factory.LoadHistory(ctx, id)
			if err != nil || len(history) == 0 || history[0].Content != query {
				t.Fatalf("durable user history changed: %+v err=%v", history, err)
			}
		})
	}
}

func TestFileReferencesRespectVisibleToolsAndInputHeadroom(t *testing.T) {
	ctx := context.Background()
	factory := profileFactory(t, memorystore.New(), &fileReferenceTestModel{})
	factory.tokenizer = tokenizer.NewApproximate("test")
	factory.config.ContextWindowTokens = 1200
	opening := schema.UserMessage("Read @design.md")
	m := &recordingModel{factory: factory}
	inspections := 0
	factory.config.InspectFileReferences = func(context.Context, *tool.Pipeline, tool.Call, string) ([]FileReference, error) {
		inspections++
		return []FileReference{{Path: "design.md", SizeBytes: 678, Headings: []FileHeading{{Title: strings.Repeat("heading text ", 2000)}}}}, nil
	}
	input := []*schema.Message{opening}
	if view, err := m.attachFileReferences(ctx, input, opening, nil); err != nil || view[0] != opening || inspections != 0 {
		t.Fatalf("a hidden workspace_read allowed inspection: count=%d err=%v", inspections, err)
	}
	options := []model.Option{model.WithTools([]*schema.ToolInfo{{Name: "workspace_read", Desc: "read"}})}
	view, err := m.attachFileReferences(ctx, input, opening, options)
	if err != nil || !strings.Contains(view[0].Content, `"headings_truncated":true`) || strings.Contains(view[0].Content, "heading text") {
		t.Fatalf("did not reduce overlong metadata: view=%+v err=%v", view, err)
	}
	limit := modelcontext.InputLimit(factory.config.ContextWindowTokens, requestOutputLimit(factory, options))
	if tokens := m.countRequestInput(view, options); tokens > limit {
		t.Fatalf("metadata sent %d tokens with a limit of %d", tokens, limit)
	}
	for m.countRequestInput(input, options) < limit-12 {
		opening.Content += " word"
	}
	if m.countRequestInput(input, options) > limit {
		t.Fatal("test base prompt did not fit")
	}
	if view, err := m.attachFileReferences(ctx, input, opening, options); err != nil || view[0] != opening {
		t.Fatalf("nearly full prompt unexpectedly received metadata: err=%v", err)
	}
}

type fileReferenceTestModel struct {
	failFirst bool
	inputs    [][]*schema.Message
}

func (m *fileReferenceTestModel) Generate(_ context.Context, input []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	stable, err := cloneJournalInput(input)
	if err != nil {
		return nil, err
	}
	m.inputs = append(m.inputs, stable)
	call := len(m.inputs)
	if m.failFirst && call == 1 {
		return nil, errors.New("provider temporarily unavailable")
	}
	if call == 1 || (m.failFirst && call == 2) {
		return schema.AssistantMessage("", []schema.ToolCall{{ID: "read-1", Type: "function", Function: schema.FunctionCall{Name: "workspace_read", Arguments: `{}`}}}), nil
	}
	return schema.AssistantMessage("finished", nil), nil
}

func (m *fileReferenceTestModel) Stream(ctx context.Context, input []*schema.Message, options ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	message, err := m.Generate(ctx, input, options...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{message}), nil
}

type fileReferenceJournalFailure struct {
	session.EventStore
	prepared int
}

func (s *fileReferenceJournalFailure) Append(ctx context.Context, id session.ID, sequence uint64, events ...session.PendingEvent) ([]session.Event, error) {
	for _, event := range events {
		if event.Type == session.EventTurnPrepared {
			s.prepared++
			if s.prepared == 2 {
				return nil, errors.New("temporarily unable to store prepared model request")
			}
		}
	}
	return s.EventStore.Append(ctx, id, sequence, events...)
}
