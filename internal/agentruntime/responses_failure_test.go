package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/modelgateway"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/memorystore"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

func TestTruncatedResponsesCannotExecuteStreamedToolCall(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"id\":\"item\",\"call_id\":\"call\",\"name\":\"write\"}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":0,\"item_id\":\"item\",\"delta\":\"{}\"}\n\n")
	}))
	defer server.Close()
	t.Setenv("GSEA_TEST_SSE_KEY", "local-test-only")
	fake, err := modelgateway.New(context.Background(), modelgateway.Config{Provider: modelgateway.ProviderOpenAI, WireAPI: modelgateway.WireAPIResponses, BaseURL: server.URL, Model: "test", APIKeyEnvironment: "GSEA_TEST_SSE_KEY", Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	store := memorystore.New()
	factory := profileFactory(t, store, fake, profileBinding("write", tool.RiskWrite, func(context.Context, json.RawMessage) (json.RawMessage, error) {
		calls.Add(1)
		return json.RawMessage(`{}`), nil
	}))
	turn, err := factory.NewTurn(context.Background(), "truncated")
	if err != nil {
		t.Fatal(err)
	}
	_, err = turn.Run(context.Background(), []*schema.Message{schema.UserMessage("write something")}, nil)
	if !errors.Is(err, modelgateway.ErrResponsesStreamTruncated) || calls.Load() != 0 {
		t.Fatalf("partial tool escaped: calls=%d err=%v", calls.Load(), err)
	}
	events, err := store.Load(context.Background(), "truncated", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == session.EventTurnCompleted || event.Type == session.EventToolStarted {
			t.Fatalf("partial response committed %s", event.Type)
		}
	}
}
