package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
)

func TestHeadlessHumanOutputFiltersSplitReasoningTerminalCommands(t *testing.T) {
	var writer bytes.Buffer
	output := newTurnOutput(&writer, false)
	for _, delta := range []string{"reason\x1b]52;", "c;clipboard\x07 end"} {
		if err := output.Sink(context.Background(), agentruntime.Event{Type: agentruntime.EventAssistantReasoningDelta, Delta: delta}); err != nil {
			t.Fatal(err)
		}
	}
	if got := writer.String(); strings.ContainsRune(got, '\x1b') || strings.Contains(got, "clipboard") || !strings.Contains(got, "reason end") {
		t.Fatalf("untrusted terminal command passed through: %q", got)
	}
}
