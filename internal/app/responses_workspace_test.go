package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/modelgateway"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

// Exercise the exact failure from the pasted transcript: a complete streamed
// Responses function call must reach the real workspace reader as an object.
func TestResponsesCompleteCallReadsActualWorkspace(t *testing.T) {
	ctx := context.Background()
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "runtime.go"), []byte("package agentflow\n// workspace fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	application, err := Open(ctx, config.Config{DataDirectory: t.TempDir(), WorkspaceDirectory: workspace})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.Close() })
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			http.NotFound(w, r)
			return
		}
		var request struct {
			Input []struct {
				Type    string `json:"type"`
				Output  string `json:"output"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			http.Error(w, "bad input", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			encoded, _ := json.Marshal(request)
			if !strings.Contains(string(encoded), application.workspace.Root()) || !strings.Contains(string(encoded), "Host workspace context") {
				t.Errorf("effective workspace missing from model input: %s", encoded)
			}
			fmt.Fprint(w, "data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"function_call\",\"id\":\"item-read\",\"call_id\":\"read\",\"name\":\"workspace_read\",\"arguments\":\"{\\\"path\\\":\\\"runtime.go\\\"}\"}}\n\n")
		} else {
			found := false
			for _, item := range request.Input {
				if item.Type == "function_call_output" && strings.Contains(item.Output, "workspace fixture") && !strings.Contains(item.Output, `"error"`) {
					found = true
				}
			}
			if !found {
				t.Error("reader result did not reach the second model request")
			}
			fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"read successfully\"}\n\n")
		}
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
	}))
	defer server.Close()
	t.Setenv("GSEA_TEST_WORKSPACE_KEY", "local-test-only")
	chat, err := modelgateway.New(ctx, modelgateway.Config{Provider: modelgateway.ProviderOpenAI, WireAPI: modelgateway.WireAPIResponses, Model: "test", BaseURL: server.URL, APIKeyEnvironment: "GSEA_TEST_WORKSPACE_KEY", Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	root, _, err := application.EnsureRootThread(ctx, "workspace-stream")
	if err != nil {
		t.Fatal(err)
	}
	factory, err := application.NewAgentFactory(chat, agentruntime.Config{ActorScope: root.ActorScope(), MaxOutputTokens: 256})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := factory.NewTurn(ctx, "workspace-stream")
	if err != nil {
		t.Fatal(err)
	}
	result, err := turn.Run(ctx, []*schema.Message{schema.UserMessage("Read runtime.go without modifying it.")}, nil)
	if err != nil || result.Final == nil || result.Final.Content != "read successfully" || calls.Load() != 2 {
		t.Fatalf("result=%+v calls=%d err=%v", result, calls.Load(), err)
	}
	events, err := application.events.Load(ctx, "workspace-stream", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == session.EventToolFailed {
			t.Fatalf("workspace tool failed: %s", event.Payload)
		}
	}
}
