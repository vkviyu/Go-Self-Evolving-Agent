package tui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
)

func TestCancelledOperationDeliversCompletionAfterFullEventQueue(t *testing.T) {
	operation, cancel := context.WithCancel(context.Background())
	lifetime, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	events := make(chan tea.Msg, 1)
	events <- runtimeEventMsg{}
	cancel()
	command := runOperationCommand(operation, func(context.Context, agentruntime.EventSink) (agentruntime.Result, error) {
		return agentruntime.Result{}, context.Canceled
	}, events, lifetime)
	done := make(chan struct{})
	go func() { command(); close(done) }()
	<-events
	select {
	case event := <-events:
		completion, ok := event.(turnDoneMsg)
		if !ok || !errors.Is(completion.err, context.Canceled) {
			t.Fatalf("completion lost/reordered: %#v", event)
		}
	case <-lifetime.Done():
		t.Fatal("full queue lost turn completion")
	}
	<-done
}

func TestFileLinksDoNotResolveOutsideWorkspace(t *testing.T) {
	workspace, outside := t.TempDir(), t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workspace, "escape")); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{secret, "../secret.txt", "escape/secret.txt"} {
		if _, ok := resolveFileToken(candidate, workspace); ok {
			t.Fatalf("external path was linked: %s", candidate)
		}
	}
}

func TestTranscriptStripsUntrustedTerminalCommands(t *testing.T) {
	model := &Model{styles: defaultStyles()}
	model.viewport.SetWidth(80)
	for _, block := range []transcriptBlock{
		{kind: blockReasoning, content: "visible\x1b]52;c;secret\x07"},
		{kind: blockFailure, content: "visible\x1b[2J\x08"},
		{kind: blockTool, toolName: "tool\x1b]52;c;secret\x07", toolResult: `{"error":"visible\u001b]52;c;secret\u0007"}`, resultExpanded: true},
	} {
		output := model.renderBlock(&block)
		if strings.Contains(output, "\x1b]52;") || strings.Contains(output, "\x1b[2J") || strings.Contains(output, "\x08") {
			t.Fatalf("terminal command was rendered: %q", output)
		}
	}
}

func TestInputDragUsesGraphemeColumnsAfterWrapping(t *testing.T) {
	model := newTestModel(t)
	model.resize(28, 18)
	model.input.Focus()
	prefix := strings.Repeat("z", model.input.Width())
	const cluster = "👩‍💻"
	model.input.SetValue(prefix + cluster + "e\u0301")
	model.resizeInput()
	y := model.inputTopRow() + 1
	x := 1 + ansi.StringWidth(model.input.Prompt)
	model.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	model.Update(tea.MouseReleaseMsg{X: x + 2, Y: y, Button: tea.MouseLeft})
	if got := model.input.SelectedText(); got != cluster {
		t.Fatalf("selected %q, want intact grapheme %q", got, cluster)
	}
}

func BenchmarkLongInputLayout(b *testing.B) {
	model, err := NewModel(context.Background(), Config{Conversation: &fakeConversation{}})
	if err != nil {
		b.Fatal(err)
	}
	model.resize(80, 24)
	model.input.SetValue(strings.Repeat("word ", 20_000))
	for b.Loop() {
		model.resizeInput()
	}
}
