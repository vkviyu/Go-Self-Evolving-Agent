package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/conversation"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

func TestResponsesStreamDiagnosticsAndResumeThroughCLI(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Second)
	defer cancel()
	data, workspace := t.TempDir(), t.TempDir()
	cfg := cliCommandConfig(t, data, workspace)
	dataAlias := filepath.Join(t.TempDir(), "runtime-alias")
	if err := os.Symlink(data, dataAlias); err != nil {
		t.Fatal(err)
	}
	const key, secret = "GSEA_TEST_RESPONSES_DIAGNOSTICS_KEY", "local-only-test-credential"
	t.Setenv(key, secret)

	const sessionID session.ID = "responses-stream-diagnostics"
	const fixtureBody = "durable fixture"
	if err := os.WriteFile(filepath.Join(workspace, "fixture.txt"), []byte(fixtureBody+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "gsea")
	if out, err := exec.CommandContext(ctx, "go", "build", "-o", binary, "../../cmd/gsea").CombinedOutput(); err != nil {
		t.Fatalf("build GSEA: %v\n%s", err, out)
	}

	const (
		failedProviderID    = "resp-diagnostics-idle"
		recoveredProviderID = "resp-diagnostics-recovered"
		partial             = "unconfirmed reply prefix"
		prompt              = "Read fixture.txt and summarize it."
	)
	var calls atomic.Int32
	requests := make(chan []byte, 3)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			http.NotFound(w, r) // Optional discovery of model metadata.
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/responses" || r.Header.Get("Authorization") != "Bearer "+secret {
			t.Error("unexpected Responses request")
			http.Error(w, "unexpected Responses request", http.StatusBadRequest)
			return
		}
		payload, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read fixture request: %v", err)
			http.Error(w, "cannot read request", http.StatusBadRequest)
			return
		}
		n := calls.Add(1)
		if n > 3 {
			t.Errorf("unexpected Responses request %d", n)
			http.Error(w, "unexpected request", http.StatusConflict)
			return
		}
		requests <- payload
		w.Header().Set("Content-Type", "text/event-stream")
		switch n {
		case 1:
			fmt.Fprint(w, `data: {"type":"response.output_item.done","item":{"type":"function_call","id":"fixture-item","call_id":"fixture-read","name":"workspace_read","arguments":"{\"path\":\"fixture.txt\"}"}}`+"\n\n")
		case 2:
			w.Header().Set("X-Request-ID", failedProviderID)
			fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\n", partial)
			if err := http.NewResponseController(w).Flush(); err != nil {
				t.Errorf("flush partial model stream: %v", err)
				return
			}
			// Keep the body open so this is a read-idle timeout, rather than EOF.
			<-r.Context().Done()
			return
		case 3:
			w.Header().Set("X-Request-ID", recoveredProviderID)
			fmt.Fprint(w, `data: {"type":"response.output_text.delta","delta":"confirmed answer"}`+"\n\n")
		}
		fmt.Fprint(w, `data: {"type":"response.completed","response":{"status":"completed"}}`+"\n\n")
	}))
	defer upstream.Close()

	base := []string{
		"chat", "--workspace", workspace, "--data-dir", dataAlias,
		"--session", string(sessionID), "--provider", "openai", "--wire-api", "responses",
		"--model", "fixture-model", "--base-url", upstream.URL, "--api-key-env", key,
		"--timeout", "1s", "--headless-format", "json",
	}
	runChat := func(input string, flags ...string) (string, string, error) {
		command := exec.CommandContext(ctx, binary, append(append([]string(nil), base...), flags...)...)
		command.Stdin = strings.NewReader(input)
		var stdout, stderr bytes.Buffer
		command.Stdout, command.Stderr = &stdout, &stderr
		err := command.Run()
		return stdout.String(), stderr.String(), err
	}
	stdout, stderr, err := runChat(prompt, "--prompt-file", "-")
	if err == nil || stdout != "" || !strings.Contains(stderr, "execution paused (transport)") ||
		!strings.Contains(stderr, "model stream read timed out after 1s") ||
		!strings.Contains(stderr, "last SSE event response.output_text.delta") || calls.Load() != 2 {
		t.Fatalf("expected a body-read timeout after one confirmed tool: stdout=%q stderr=%q requests=%d err=%v", stdout, stderr, calls.Load(), err)
	}
	// A repeated workspace read would now produce different model input.
	if err := os.WriteFile(filepath.Join(workspace, "fixture.txt"), []byte("changed on disk while paused\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err = runChat("", "--list-recoveries")
	if err != nil {
		t.Fatalf("list recoveries: %v\n%s", err, stderr)
	}
	var recoveries []conversation.Recovery
	if err := json.Unmarshal([]byte(stdout), &recoveries); err != nil ||
		len(recoveries) != 1 || recoveries[0].TurnID == "" || recoveries[0].Phase != "execution" || calls.Load() != 2 {
		t.Fatalf("expected one original interrupted turn without a provider call: %q, %v", stdout, err)
	}
	turnID := recoveries[0].TurnID
	stdout, stderr, err = runChat("", "--resume-turn", turnID)
	var result headlessJSONResult
	if err != nil || json.Unmarshal([]byte(stdout), &result) != nil || result.Schema != "gsea.chat.result.v1" ||
		result.Content != "confirmed answer" || calls.Load() != 3 {
		t.Fatalf("resume original turn: stdout=%q stderr=%q requests=%d err=%v", stdout, stderr, calls.Load(), err)
	}
	initial, failed, recovered := <-requests, <-requests, <-requests
	if bytes.Contains(initial, []byte(fixtureBody)) || !bytes.Contains(failed, []byte(fixtureBody)) || !bytes.Equal(failed, recovered) {
		t.Fatal("expected original failed and resumed requests to carry the same confirmed tool output")
	}

	// Both CLI processes have closed their owners before the test reads ATE.
	database := openCLIApp(t, cfg)
	t.Cleanup(func() { _ = database.Close() })
	events, err := database.Events().Load(ctx, sessionID, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	checkResponsesDiagnosticsHistory(t, events, turnID, failedProviderID, recoveredProviderID, partial, secret)
}

func checkResponsesDiagnosticsHistory(t *testing.T, events []session.Event, turnID, failedProviderID, recoveredProviderID, partial, secret string) {
	t.Helper()
	type transport struct {
		RequestID   string    `json:"request_id"`
		Status      int       `json:"status"`
		ContentType string    `json:"content_type"`
		BodyBytes   int       `json:"body_bytes"`
		StartedAt   time.Time `json:"started_at"`
		HeadersAt   time.Time `json:"headers_at"`
		FirstByteAt time.Time `json:"first_byte_at"`
		LastByteAt  time.Time `json:"last_byte_at"`
		EndedAt     time.Time `json:"ended_at"`
		LastEvent   string    `json:"last_event"`
		Completed   bool      `json:"completed"`
		Outcome     string    `json:"outcome"`
	}
	var (
		toolResults, userTurns, completedTurns int
		failedSeen, recoveredSeen              bool
		postToolRequest                        string
	)
	for _, event := range events {
		switch event.Type {
		case session.EventUserMessage:
			userTurns++
		case session.EventTurnCompleted:
			if event.CorrelationID != turnID {
				t.Errorf("completed a different turn %q", event.CorrelationID)
			}
			completedTurns++
		case session.EventToolSucceeded:
			toolResults++
		case session.EventModelRequested:
			if toolResults != 1 || failedSeen {
				continue
			}
			var payload struct {
				RequestID string `json:"request_id"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			postToolRequest = payload.RequestID
		case session.EventModelFailed, session.EventAssistantMessage:
			var payload struct {
				RequestID string          `json:"request_id"`
				TurnID    string          `json:"turn_id"`
				Transport json.RawMessage `json:"transport"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if len(payload.Transport) == 0 {
				continue
			}
			var diagnostic transport
			if err := json.Unmarshal(payload.Transport, &diagnostic); err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(payload.Transport, []byte(partial)) || bytes.Contains(payload.Transport, []byte(secret)) {
				t.Fatal("persisted transport diagnostics exposed content or a credential")
			}
			switch diagnostic.RequestID {
			case failedProviderID:
				failedSeen = true
				if event.Type != session.EventModelFailed || payload.RequestID == "" || payload.RequestID != postToolRequest ||
					payload.TurnID != turnID || event.CorrelationID != turnID || diagnostic.Status != http.StatusOK ||
					diagnostic.ContentType != "text/event-stream" || diagnostic.BodyBytes == 0 || diagnostic.Completed ||
					diagnostic.LastEvent != "response.output_text.delta" || diagnostic.Outcome != "stream_read_timeout" ||
					diagnostic.StartedAt.IsZero() || diagnostic.HeadersAt.Before(diagnostic.StartedAt) ||
					diagnostic.FirstByteAt.Before(diagnostic.HeadersAt) || diagnostic.LastByteAt.Before(diagnostic.FirstByteAt) ||
					!diagnostic.EndedAt.After(diagnostic.LastByteAt) {
					t.Fatalf("invalid persisted post-tool timeout diagnostic: %+v", diagnostic)
				}
			case recoveredProviderID:
				recoveredSeen = true
				if event.Type != session.EventAssistantMessage || event.CorrelationID != turnID || !diagnostic.Completed ||
					diagnostic.LastEvent != "response.completed" || diagnostic.Outcome != "completed" {
					t.Fatalf("invalid persisted resumed-response diagnostic: %+v", diagnostic)
				}
			}
		}
	}
	if toolResults != 1 || userTurns != 1 || completedTurns != 1 || !failedSeen || !recoveredSeen {
		t.Fatalf("unexpected durable results: tools=%d user_turns=%d completed_turns=%d timeout=%t recovered=%t",
			toolResults, userTurns, completedTurns, failedSeen, recoveredSeen)
	}
}
