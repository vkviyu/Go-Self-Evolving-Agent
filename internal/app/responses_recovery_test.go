package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/conversation"
	"github.com/vkviyu/go-self-evolving-agent/internal/modelgateway"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

func TestTruncatedResponsesStreamPausesAndResumesOriginalTurn(t *testing.T) {
	t.Run("truncated", func(t *testing.T) {
		testInterruptedResponsesRecovery(t, false)
	})
	t.Run("timeout_retries_automatically", func(t *testing.T) {
		testInterruptedResponsesRecovery(t, true)
	})
}

func testInterruptedResponsesRecovery(t *testing.T, timeout bool) {
	t.Helper()
	ctx := t.Context()
	appConfig := config.Config{DataDirectory: t.TempDir(), WorkspaceDirectory: t.TempDir()}

	var calls atomic.Int32
	requests := make(chan []byte, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			http.NotFound(w, r) // Context metadata probe.
			return
		}
		payload, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		switch calls.Add(1) {
		case 1:
			requests <- payload
			fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"unconfirmed text\"}\n\n")
			if timeout {
				if err := http.NewResponseController(w).Flush(); err != nil {
					t.Error(err)
					return
				}
				<-r.Context().Done()
			}
		case 2:
			requests <- payload
			fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"confirmed answer\"}\n\n")
			fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
		default:
			t.Error("unexpected model request")
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer server.Close()
	t.Setenv("GSEA_TEST_RECOVERY_KEY", "local-test-only")
	chat, err := modelgateway.New(ctx, modelgateway.Config{Provider: modelgateway.ProviderOpenAI, WireAPI: modelgateway.WireAPIResponses, Model: "test", BaseURL: server.URL, APIKeyEnvironment: "GSEA_TEST_RECOVERY_KEY", Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	const id session.ID = "stream-recovery"
	openChat := func() (*App, *conversation.Conversation) {
		t.Helper()
		application, err := testOpen(t, ctx, appConfig)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = application.Close() })
		root, _, err := application.EnsureRootThread(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		factory, err := application.NewAgentFactory(chat, agentruntime.Config{ActorScope: root.ActorScope(), MaxOutputTokens: 256})
		if err != nil {
			t.Fatal(err)
		}
		chatSession, err := conversation.Open(ctx, factory, id)
		if err != nil {
			t.Fatal(err)
		}
		return application, chatSession
	}
	application, chatSession := openChat()
	result, err := chatSession.RunTurn(ctx, "Explain the parser.", nil)
	if !timeout {
		kind, paused := orchestration.ExecutionStopKindOf(err)
		if !paused || kind != orchestration.ExecutionTransport || result.Final != nil {
			t.Fatalf("incomplete model stream was not paused: result=%+v err=%v", result, err)
		}
		if err := application.Close(); err != nil {
			t.Fatal(err)
		}
		_, chatSession = openChat()
		items, listErr := chatSession.Recoveries(ctx)
		if listErr != nil || len(items) != 1 || items[0].Phase != "execution" || items[0].TurnID == "" {
			t.Fatalf("interrupted turn not recoverable: %+v err=%v", items, listErr)
		}
		result, err = chatSession.ResumeTurn(ctx, items[0].TurnID, nil)
	}
	if err != nil || result.Final == nil || result.Final.Content != "confirmed answer" || calls.Load() != 2 {
		t.Fatalf("unexpected recovery: result=%+v calls=%d err=%v", result, calls.Load(), err)
	}
	if first, second := <-requests, <-requests; !bytes.Equal(first, second) {
		t.Fatal("retry changed the durable model request")
	}
	history := chatSession.History()
	if len(history) != 2 || history[0].Content != "Explain the parser." || history[1].Content != "confirmed answer" {
		t.Fatalf("recovery duplicated the question or committed partial output: %+v", history)
	}
	if items, err := chatSession.Recoveries(ctx); err != nil || len(items) != 0 {
		t.Fatalf("completed turn still listed for recovery: %+v err=%v", items, err)
	}
}

func TestTruncatedResponsesAfterConfirmedToolResumesWithoutRepeatingTool(t *testing.T) {
	ctx := t.Context()
	appConfig := config.Config{
		DataDirectory:      t.TempDir(),
		WorkspaceDirectory: t.TempDir(),
		ATE: config.ATEConfig{
			Enabled:   true,
			Binary:    temporalBinary(t),
			Directory: t.TempDir(),
			Timeout:   30 * time.Second,
		},
	}
	if err := os.WriteFile(appConfig.WorkspaceDirectory+"/fixture.txt", []byte("durable fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int32
	requests := make(chan []byte, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			http.NotFound(w, r)
			return
		}
		payload, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for stage, response := range map[string]string{
			"Stage: discover":    `{"space_ids":[],"new_space":null,"search_terms":[],"full_history":false}`,
			"Stage: history-map": `{"notes":[]}`,
			"Stage: extract":     `{"lessons":[]}`,
			"Stage: select":      `{"memory_ids":[]}`,
			"Stage: verify":      `{"passed":false,"reason":"no candidate","event_ids":[],"claims_tool_success":false,"tool_success_event_ids":[]}`,
			"Stage: organize":    `{"actions":[]}`,
		} {
			if bytes.Contains(payload, []byte(stage)) {
				if !bytes.Contains(payload, []byte(`"stream":true`)) {
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprintf(w, `{"id":"auxiliary","status":"completed","output_text":%q,"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":%q}]}]}`, response, response)
					return
				}
				fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\n", response)
				fmt.Fprint(w, `data: {"type":"response.completed","response":{"status":"completed"}}`+"\n\n")
				return
			}
		}
		requests <- payload
		switch calls.Add(1) {
		case 1:
			fmt.Fprint(w, `data: {"type":"response.output_item.done","item":{"type":"function_call","id":"item-1","call_id":"call-1","name":"workspace_read","arguments":"{\"path\":\"fixture.txt\"}"}}`+"\n\n")
			fmt.Fprint(w, `data: {"type":"response.completed","response":{"status":"completed"}}`+"\n\n")
		case 2:
			// The tool result has already been durably recorded. Lose only the
			// following model response, so recovery must replay this request.
			fmt.Fprint(w, `data: {"type":"response.output_text.delta","delta":"partial"}`+"\n\n")
		case 3:
			fmt.Fprint(w, `data: {"type":"response.output_text.delta","delta":"recovered answer"}`+"\n\n")
			fmt.Fprint(w, `data: {"type":"response.completed","response":{"status":"completed"}}`+"\n\n")
		default:
			t.Error("unexpected model request")
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer server.Close()
	t.Setenv("GSEA_TEST_RECOVERY_KEY", "local-test-only")
	chat, err := modelgateway.New(ctx, modelgateway.Config{
		Provider:          modelgateway.ProviderOpenAI,
		WireAPI:           modelgateway.WireAPIResponses,
		Model:             "test",
		BaseURL:           server.URL,
		APIKeyEnvironment: "GSEA_TEST_RECOVERY_KEY",
		Timeout:           time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	const id session.ID = "tool-stream-recovery"
	openChat := func() (*App, *conversation.Conversation) {
		t.Helper()
		application, err := testOpen(t, ctx, appConfig)
		if err != nil {
			t.Fatal(err)
		}
		root, _, err := application.EnsureRootThread(ctx, id)
		if err != nil {
			_ = application.Close()
			t.Fatal(err)
		}
		factory, err := application.NewAgentFactory(chat, agentruntime.Config{
			ActorScope: root.ActorScope(), MaxOutputTokens: 256,
		})
		if err != nil {
			_ = application.Close()
			t.Fatal(err)
		}
		chatSession, err := conversation.Open(ctx, factory, id)
		if err != nil {
			_ = application.Close()
			t.Fatal(err)
		}
		return application, chatSession
	}

	application, chatSession := openChat()
	result, err := chatSession.RunTurn(ctx, "Read fixture.txt and summarize it.", nil)
	kind, paused := orchestration.ExecutionStopKindOf(err)
	if !paused || kind != orchestration.ExecutionTransport || result.Final != nil {
		_ = application.Close()
		t.Fatalf("tool-followup interruption was not paused: result=%+v err=%v", result, err)
	}
	if err := application.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, chatSession := openChat()
	items, err := chatSession.Recoveries(ctx)
	if err != nil || len(items) != 1 {
		_ = reopened.Close()
		t.Fatalf("interrupted tool turn not recoverable: %+v err=%v", items, err)
	}
	result, err = chatSession.ResumeTurn(ctx, items[0].TurnID, nil)
	if err != nil || result.Final == nil || result.Final.Content != "recovered answer" || calls.Load() != 3 {
		_ = reopened.Close()
		t.Fatalf("unexpected tool recovery: result=%+v calls=%d err=%v", result, calls.Load(), err)
	}
	first, second, third := <-requests, <-requests, <-requests
	if bytes.Equal(first, second) || !bytes.Equal(second, third) {
		_ = reopened.Close()
		t.Fatal("recovery did not preserve the interrupted post-tool request")
	}
	successes := 0
	if err := session.VisitEvents(ctx, reopened.events, id, func(event session.Event) error {
		if event.Type == session.EventToolSucceeded {
			successes++
		}
		return nil
	}); err != nil {
		_ = reopened.Close()
		t.Fatal(err)
	}
	if successes != 1 {
		_ = reopened.Close()
		t.Fatalf("recovery repeated or lost the confirmed tool effect: successes=%d", successes)
	}
	history := chatSession.History()
	encodedHistory, _ := json.Marshal(history)
	if len(history) != 4 || history[0].Content != "Read fixture.txt and summarize it." ||
		history[len(history)-1].Content != "recovered answer" || bytes.Contains(encodedHistory, []byte("partial")) ||
		bytes.Count(encodedHistory, []byte("Read fixture.txt and summarize it.")) != 1 {
		_ = reopened.Close()
		t.Fatalf("recovery duplicated partial output or input: %+v", history)
	}
	if items, err := chatSession.Recoveries(ctx); err != nil || len(items) != 0 {
		_ = reopened.Close()
		t.Fatalf("completed tool turn still listed for recovery: %+v err=%v", items, err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestResponsesSlowFirstByteKeepsATEAvailable(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	cfg := config.Config{ATE: config.ATEConfig{Timeout: 1500 * time.Millisecond}}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Error(err)
			return
		}
		select {
		case <-time.After(2200 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"slow answer\"}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
	}))
	defer server.Close()
	t.Setenv("GSEA_TEST_RECOVERY_KEY", "local-test-only")
	backend, err := modelgateway.New(ctx, modelgateway.Config{
		Provider: modelgateway.ProviderOpenAI, WireAPI: modelgateway.WireAPIResponses, Model: "test",
		BaseURL: server.URL, APIKeyEnvironment: "GSEA_TEST_RECOVERY_KEY", StreamIdleTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	app, err := testOpen(t, ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	const id session.ID = "slow-first-byte"
	root, _, err := app.EnsureRootThread(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	factory, err := app.NewAgentFactory(backend, agentruntime.Config{ActorScope: root.ActorScope(), MaxOutputTokens: 256})
	if err != nil {
		t.Fatal(err)
	}
	conversation, err := conversation.Open(ctx, factory, id)
	if err != nil {
		t.Fatal(err)
	}
	result, err := conversation.RunTurn(ctx, "Explain this.", nil)
	if err != nil || requests.Load() != 1 || result.Final == nil || result.Final.Content != "slow answer" {
		t.Fatalf("valid slow response could not finish: requests=%d result=%+v err=%v", requests.Load(), result, err)
	}
}
