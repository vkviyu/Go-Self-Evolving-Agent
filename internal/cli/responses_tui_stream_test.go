package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentmanager"
	"github.com/vkviyu/go-self-evolving-agent/internal/app"
	"github.com/vkviyu/go-self-evolving-agent/internal/observation"
	"github.com/vkviyu/go-self-evolving-agent/internal/tui"
)

func TestResponsesDeltasReachTUIBeforeCompletion(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	cfg := cliCommandConfig(t, t.TempDir(), t.TempDir())
	const key = "GSEA_TEST_TUI_STREAM_KEY"
	t.Setenv(key, "local-responses-fixture")
	const reasoning, answer = "ReasoningStreamCheck", "AnswerStreamCheck"
	releaseReasoning := make(chan struct{})
	releaseAnswer := make(chan struct{})
	releaseCompletion := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			http.NotFound(w, r) // Optional model metadata lookup.
			return
		}
		var request struct{ Stream bool }
		if r.Method != http.MethodPost || r.URL.Path != "/responses" ||
			json.NewDecoder(r.Body).Decode(&request) != nil || !request.Stream {
			t.Error("expected a streaming Responses request")
			http.Error(w, "expected a streaming Responses request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		send := func(event string) bool {
			if _, err := fmt.Fprintf(w, "data: %s\n\n", event); err != nil {
				t.Errorf("write fixture event: %v", err)
				return false
			}
			if err := http.NewResponseController(w).Flush(); err != nil {
				t.Errorf("flush fixture event: %v", err)
				return false
			}
			return true
		}
		wait := func(release <-chan struct{}) bool {
			select {
			case <-release:
				return true
			case <-r.Context().Done():
				return false
			case <-ctx.Done():
				return false
			}
		}
		if send(`{"type":"response.created","response":{"status":"in_progress"}}`) &&
			wait(releaseReasoning) && send(fmt.Sprintf(`{"type":"response.reasoning_summary_text.delta","delta":%q}`, reasoning)) &&
			wait(releaseAnswer) && send(fmt.Sprintf(`{"type":"response.output_text.delta","delta":%q}`, answer)) &&
			wait(releaseCompletion) {
			send(`{"type":"response.completed","response":{"status":"completed"}}`)
		}
	}))
	t.Cleanup(upstream.Close)

	options, err := parseChatOptions([]string{
		"--provider", "openai", "--wire-api", "responses", "--model", "fixture-model",
		"--base-url", upstream.URL, "--api-key-env", key, "--timeout", "20s",
		"--max-iterations", "20", "--max-running-agents", "4",
	}, cfg, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	application, err := app.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.Close() })
	chatModel, err := productionDependencies.newModel(ctx, options.model)
	if err != nil {
		t.Fatal(err)
	}
	root, _, err := application.EnsureRootThread(ctx, options.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	factory, err := application.NewAgentFactory(chatModel, rootRuntimeConfig(options, root.ActorScope(), application.Orchestration()))
	if err != nil {
		t.Fatal(err)
	}
	supervisor, err := application.NewSupervisor(agentmanager.Config{RunContext: ctx, Runner: newChildRunner(factory)})
	if err != nil {
		t.Fatal(err)
	}
	rootChat, err := openRootConversation(ctx, factory, supervisor, root)
	if err != nil {
		t.Fatal(err)
	}
	displayCtx, finish := observeChat(ctx, cfg.DataDirectory, options.sessionID, true, io.Discard)
	// Exercise the TUI's own operation context and check the trace once it drains.
	t.Cleanup(func() {
		finish(nil)
		if !t.Failed() {
			checkTUITrace(t, cfg.DataDirectory, string(options.sessionID))
		}
	})
	tuiModel, err := tui.NewModel(displayCtx, tui.Config{Conversation: rootChat, Workspace: cfg.WorkspaceDirectory})
	if err != nil {
		t.Fatal(err)
	}
	output := &responsesTUIOutput{changed: make(chan struct{}, 1)}
	program := tea.NewProgram(tuiModel, tea.WithContext(ctx), tea.WithInput(nil), tea.WithOutput(output), tea.WithoutSignalHandler())
	var programErr error
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		_, programErr = program.Run()
	}()
	defer func() { cancel(); <-finished }()

	program.Send(tea.WindowSizeMsg{Width: 140, Height: 40})
	output.waitFor(t, ctx, 0, "Ask GSEA")
	program.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("Count the source files in this workspace.")})
	program.Send(tea.KeyMsg{Type: tea.KeyEnter})
	output.waitFor(t, ctx, 0, "Waiting for model output")
	close(releaseReasoning)
	output.waitFor(t, ctx, 0, reasoning)
	close(releaseAnswer)
	beforeCompletion := output.waitFor(t, ctx, 0, answer)
	close(releaseCompletion)
	output.waitFor(t, ctx, beforeCompletion, "Ready")
	program.Quit()
	<-finished
	if programErr != nil || rootChat.CompletedTurns() != 1 {
		t.Fatalf("TUI error = %v, completed turns = %d", programErr, rootChat.CompletedTurns())
	}
}

func checkTUITrace(t *testing.T, data, sessionID string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(data, "diagnostics", "*.jsonl"))
	if err != nil || len(files) != 1 {
		t.Fatalf("expected a single trace for interactive chat: count=%d err=%v", len(files), err)
	}
	trace, err := os.Open(files[0])
	if err != nil {
		t.Fatal(err)
	}
	defer trace.Close()
	decoder := json.NewDecoder(trace)
	streamTurn := ""
	blocks := map[string][]string{}
	for {
		var event observation.Event
		if err := decoder.Decode(&event); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatal(err)
		}
		if event.SessionID != sessionID || event.TurnID == "" {
			continue
		}
		switch event.Name {
		case "responses.stream":
			if event.Status == "completed" && event.RequestID != "" && event.AgentID != "" {
				streamTurn = event.TurnID
			}
		case "transcript.block.started":
			blocks[event.TurnID] = append(blocks[event.TurnID], event.Status)
		}
	}
	if streamTurn == "" || strings.Join(blocks[streamTurn], ",") != "reasoning,text" {
		t.Fatalf("missing gateway-to-TUI trace: turn=%q blocks=%v", streamTurn, blocks)
	}
}

type responsesTUIOutput struct {
	mu      sync.Mutex
	output  strings.Builder
	changed chan struct{}
}

func (output *responsesTUIOutput) Write(p []byte) (int, error) {
	output.mu.Lock()
	defer output.mu.Unlock()
	n, err := output.output.Write(p)
	select {
	case output.changed <- struct{}{}:
	default:
	}
	return n, err
}

func (output *responsesTUIOutput) waitFor(t *testing.T, ctx context.Context, after int, marker string) int {
	t.Helper()
	for {
		output.mu.Lock()
		current := output.output.String()
		output.mu.Unlock()
		if strings.Contains(current[after:], marker) {
			return len(current)
		}
		select {
		case <-output.changed:
		case <-ctx.Done():
			t.Fatalf("TUI did not render %q: last output %q", marker, current[max(0, len(current)-1200):])
		}
	}
}
