package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/conversation"
	"github.com/vkviyu/go-self-evolving-agent/internal/modelgateway"
	"github.com/vkviyu/go-self-evolving-agent/internal/modeltelemetry"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

func TestResponsesRegeneratesRejectedToolCallWithinOriginalTurn(t *testing.T) {
	for _, retryOutcome := range []string{"corrected", "rejected_again", "interrupted"} {
		t.Run(retryOutcome, func(t *testing.T) {
			ctx := t.Context()
			cfg := config.Config{DataDirectory: t.TempDir(), WorkspaceDirectory: t.TempDir()}
			const fixture = "the already confirmed file"
			if err := os.WriteFile(filepath.Join(cfg.WorkspaceDirectory, "fixture.txt"), []byte(fixture), 0600); err != nil {
				t.Fatal(err)
			}
			const repairedFixture = "the file read by the corrected call"
			if err := os.WriteFile(filepath.Join(cfg.WorkspaceDirectory, "second.txt"), []byte(repairedFixture), 0600); err != nil {
				t.Fatal(err)
			}
			var requests atomic.Int32
			var correctionPayload atomic.Pointer[[]byte]
			const retryText = "A previous tool call was rejected before execution."
			emit := func(w io.Writer, frames ...string) {
				for _, frame := range frames {
					fmt.Fprintf(w, "data: %s\n\n", frame)
				}
			}
			const completed = `{"type":"response.completed","response":{"status":"completed"}}`
			const rejected = `{"type":"response.failed","response":{"error":{"message":"basispoints tool transport: private-detail (cause_category=transport_format, cause_reason=invalid_envelope, repair_skip_reason=no_repair_callback)"}}}`
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					http.NotFound(w, r)
					return
				}
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				switch requests.Add(1) {
				case 1:
					emit(w, `{"type":"response.reasoning_summary_text.delta","delta":"confirmed reasoning"}`,
						`{"type":"response.output_item.done","item":{"type":"function_call","id":"item","call_id":"work","name":"workspace_read","arguments":"{\"path\":\"fixture.txt\"}"}}`, completed)
				case 2:
					if !bytes.Contains(raw, []byte(fixture)) || bytes.Contains(raw, []byte(retryText)) {
						t.Error("missing the confirmed tool output or premature retry guidance")
					}
					emit(w, `{"type":"response.reasoning_summary_text.delta","delta":"unconfirmed reasoning"}`,
						`{"type":"response.output_item.done","item":{"type":"function_call","id":"rejected","call_id":"cannot-run","name":"workspace_read","arguments":"{\"path\":\"fixture.txt\"}"}}`, rejected)
				case 3:
					if !bytes.Contains(raw, []byte(fixture)) || !bytes.Contains(raw, []byte(retryText)) ||
						bytes.Contains(raw, []byte("cannot-run")) || bytes.Contains(raw, []byte("unconfirmed reasoning")) ||
						bytes.Contains(raw, []byte("private-detail")) {
						t.Error("retry did not preserve the confirmed prefix and isolate the rejected response")
					}
					switch retryOutcome {
					case "corrected":
						emit(w, `{"type":"response.output_item.done","item":{"type":"function_call","id":"item-2","call_id":"corrected","name":"workspace_read","arguments":"{\"path\":\"second.txt\"}"}}`, completed)
					case "rejected_again":
						emit(w, rejected)
					case "interrupted":
						correctionPayload.Store(&raw)
					}
				case 4:
					switch retryOutcome {
					case "corrected":
						if !bytes.Contains(raw, []byte(repairedFixture)) || bytes.Contains(raw, []byte(retryText)) {
							t.Error("corrected tool result was lost or correction leaked into the next step")
						}
					case "interrupted":
						if original := correctionPayload.Load(); original == nil || !bytes.Equal(raw, *original) {
							t.Error("explicit resume changed the interrupted corrected request")
						}
					default:
						t.Error("tool format rejection was retried more than once")
					}
					if retryOutcome != "rejected_again" {
						emit(w, `{"type":"response.output_text.delta","delta":"confirmed answer"}`, completed)
					}
				default:
					t.Error("tool format rejection was retried more than once")
					http.Error(w, "unexpected request", http.StatusBadRequest)
				}
			}))
			defer upstream.Close()

			t.Setenv("GSEA_TEST_FORMAT_TOKEN", "test-only")
			backend, err := modelgateway.New(ctx, modelgateway.Config{
				Provider: modelgateway.ProviderOpenAI, WireAPI: modelgateway.WireAPIResponses,
				Model: "fixture", BaseURL: upstream.URL, APIKeyEnvironment: "GSEA_TEST_FORMAT_TOKEN", Timeout: 5 * time.Second,
			})
			if err != nil {
				t.Fatal(err)
			}
			application, err := testOpen(t, ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer application.Close()
			const id session.ID = "tool-format-recovery"
			root, _, err := application.EnsureRootThread(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			factory, err := application.NewAgentFactory(backend, agentruntime.Config{ActorScope: root.ActorScope(), MaxOutputTokens: 256})
			if err != nil {
				t.Fatal(err)
			}
			chat, err := conversation.Open(ctx, factory, id)
			if err != nil {
				t.Fatal(err)
			}

			var retries, confirmedPreviews int
			var retryTurn string
			result, err := chat.RunTurn(ctx, "Read fixture.txt and summarize it.", func(_ context.Context, event agentruntime.Event) error {
				switch event.Type {
				case agentruntime.EventModelToolFormatRetry:
					retries++
					retryTurn = event.TurnID
				case agentruntime.EventAssistantReasoningDelta:
					if event.Delta == "confirmed reasoning" {
						confirmedPreviews++
					}
				}
				return nil
			})
			wantRequests, wantTools, wantCorrected := int32(3), 1, 1
			if retryOutcome == "interrupted" {
				if !errors.Is(err, modelgateway.ErrResponsesStreamTruncated) || requests.Load() != 3 {
					t.Fatalf("interrupted correction unexpectedly finished: %v", err)
				}
				result, err = chat.ResumeTurn(ctx, retryTurn, nil)
				wantRequests++
				wantCorrected++
			}
			if retryOutcome == "corrected" {
				wantRequests++
				wantTools++
			}
			if retryOutcome == "rejected_again" {
				kind, paused := orchestration.ExecutionStopKindOf(err)
				if !paused || kind != orchestration.ExecutionToolCallFormat || !errors.Is(err, modeltelemetry.ErrToolCallFormat) || bytes.Contains([]byte(err.Error()), []byte("private-detail")) {
					t.Fatalf("second format failure did not pause with safe detail: %v", err)
				}
			} else if err != nil || result.Final == nil || result.Final.Content != "confirmed answer" || result.TurnID != retryTurn {
				t.Fatalf("expected the repaired original turn, got %+v, %v", result, err)
			}
			if requests.Load() != wantRequests || retries != 1 || confirmedPreviews != 1 || retryTurn == "" {
				t.Fatalf("calls=%d retries=%d confirmed previews=%d turn=%q", requests.Load(), retries, confirmedPreviews, retryTurn)
			}
			var turns, tools, corrected int
			if err := session.VisitEvents(ctx, application.events, id, func(event session.Event) error {
				switch event.Type {
				case session.EventTurnStarted:
					turns++
				case session.EventToolStarted:
					tools++
				case session.EventModelRequested:
					var payload struct {
						Corrected bool `json:"tool_format_corrected"`
					}
					if err := json.Unmarshal(event.Payload, &payload); err != nil {
						return err
					}
					if payload.Corrected {
						corrected++
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if turns != 1 || tools != wantTools || corrected != wantCorrected {
				t.Fatalf("durable turns=%d tools=%d corrected requests=%d", turns, tools, corrected)
			}
		})
	}
}
