package tui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
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
	model.viewport.Width = 80
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

func TestInputSpanLayoutMatchesTextareaUnicodeAndWrapping(t *testing.T) {
	for _, width := range []int{8, 20, 40} {
		for _, source := range []string{"", "hello world", "one  two    three ", strings.Repeat("z", 101), "你好 世界 你好世界 再见", "🙂👩‍💻 emoji e\u0301e\u0301 test", "abc 末尾 ", strings.Repeat(" ", 27)} {
			probe := textarea.New()
			probe.Prompt = ""
			probe.ShowLineNumbers = false
			probe.SetWidth(width)
			probe.SetValue(source)
			count := len([]rune(probe.Value()))
			height := probe.LineInfo().Height
			expected := make([]inputDisplayLine, height)
			seen := make([]bool, height)
			for column := 0; column <= count; column++ {
				probe.SetCursor(column)
				info := probe.LineInfo()
				if !seen[info.RowOffset] {
					start := min(info.StartColumn, count)
					expected[info.RowOffset] = inputDisplayLine{start: start, end: min(count, start+info.Width)}
					seen[info.RowOffset] = true
				}
			}
			lastEnd := 0
			for index := range expected {
				if !seen[index] {
					expected[index] = inputDisplayLine{start: lastEnd, end: lastEnd}
				}
				lastEnd = expected[index].end
			}
			if actual := wrapInputSpans(probe.Value(), probe.Width()); !reflect.DeepEqual(actual, expected) {
				t.Fatalf("width=%d source=%q\nactual=%+v\nexpected=%+v", width, source, actual, expected)
			}
		}
	}
}

func BenchmarkLongInputLayout(b *testing.B) {
	value := strings.Repeat("word ", 20_000)
	for b.Loop() {
		wrapInputSpans(value, 80)
	}
}
