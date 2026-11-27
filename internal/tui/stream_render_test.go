package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
)

func TestStreamingPreviewReflowsAndReconciles(t *testing.T) {
	model := newTestModel(t)
	delta := func(value string) {
		model.Update(runtimeEventMsg{event: agentruntime.Event{Type: agentruntime.EventAssistantTextDelta, Delta: value}})
	}
	var raw strings.Builder
	for _, part := range []string{
		"## 标题\n\n", "看 [出处][ref]。\n\n", "```go\nfunc read() {\n\n", "}\n```\n",
		"继续 ", "\x1b]52;c;", "private\x07", "\xe4", "\xb8\xad",
	} {
		delta(part)
		raw.WriteString(part)
		if strings.Contains(model.transcriptContent, "private") || strings.Contains(model.transcriptContent, "\x1b]52;") {
			t.Fatal("untrusted terminal command escaped into the preview")
		}
	}
	for _, width := range []int{42, 80} {
		model.resize(width, 20)
		plain := ansi.Strip(model.transcriptContent)
		if !strings.Contains(plain, "继续 中") || !strings.Contains(plain, "func read()") || ansi.StringWidth(plain[strings.LastIndexByte(plain, '\n')+1:]) > width-2 {
			t.Fatalf("lost or clipped streamed text at width %d: %q", width, plain)
		}
		if model.blocks[0].content != raw.String() {
			t.Fatal("presentation changed the original delta data")
		}
	}
	// References resolve across sections in the authoritative final document.
	final := raw.String() + "\n\n[ref]: https://example.org\n\n最终补上的一句话。"
	model.Update(runtimeEventMsg{event: agentruntime.Event{Type: agentruntime.EventAssistantMessage, Message: schema.AssistantMessage(final, nil)}})
	history := newTestModel(t)
	history.blocks = blocksFromHistory([]*schema.Message{schema.AssistantMessage(final, nil)})
	history.resize(80, 20)
	if model.transcriptContent != history.transcriptContent || model.blocks[0].stream != nil || model.blocks[0].pending {
		t.Fatal("finished preview differs from a freshly rendered history entry")
	}
}

func TestQueuedStreamPaintsDeltasBeforeInterruptedCompletion(t *testing.T) {
	model := newTestModel(t)
	model.busy = true
	model.turnEvents = make(chan tea.Msg, 8)
	msg := func(kind agentruntime.EventType, s string) runtimeEventMsg {
		return runtimeEventMsg{event: agentruntime.Event{Type: kind, Delta: s}, at: time.Now()}
	}
	model.turnEvents <- storageProgressMsg{}
	model.turnEvents <- msg(agentruntime.EventAssistantTextDelta, "第一段\n\n")
	model.turnEvents <- msg(agentruntime.EventAssistantTextDelta, "第二段")
	model.turnEvents <- turnDoneMsg{err: context.Canceled}
	_, next := model.Update(msg(agentruntime.EventAssistantReasoningDelta, "检查中"))
	if plain := ansi.Strip(model.transcriptContent); !strings.Contains(plain, "第一段") || !strings.Contains(plain, "第二段") || !model.busy {
		t.Fatalf("queued deltas failed to paint in the live frame: %q", plain)
	}
	completion, ok := next().(turnDoneMsg)
	if !ok {
		t.Fatal("completion overtaken or lost")
	}
	model.Update(completion)
	if model.busy || model.blocks[1].stream != nil || model.blocks[1].content != "第一段\n\n第二段" {
		t.Fatal("interruption discarded the displayed partial answer")
	}
}

// Measure the interactive path after scrollback and a streamed answer already
// exist. Each sample processes eight separately arriving deltas at a fixed size.
func BenchmarkGrowingTranscript(b *testing.B) {
	const paragraph = "检查每个阶段的输出，再决定下一步。 See `context.Context`.\n\n" +
		"```go\nfunc ready() bool { return true }\n```\n\n"
	for _, size := range []int{4 << 10, 32 << 10} {
		b.Run(fmt.Sprintf("%dKiB", size>>10), func(b *testing.B) {
			model, err := NewModel(context.Background(), Config{Conversation: &fakeConversation{id: "stream"}})
			if err != nil {
				b.Fatal(err)
			}
			model.resize(100, 30)
			history := strings.Repeat(paragraph, (16<<10)/len(paragraph))
			model.blocks = []transcriptBlock{{kind: blockUser, content: "上一个问题"}, {kind: blockAssistant, content: history}, {kind: blockUser, content: "当前问题"}}
			model.refreshTranscript(true)
			base := strings.Repeat(paragraph, size/len(paragraph))
			delta := runtimeEventMsg{event: agentruntime.Event{Type: agentruntime.EventAssistantTextDelta, Delta: "深入检查。"}}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				model.blocks = model.blocks[:3]
				model.activeAssistant = -1
				model.applyRuntimeEvent(agentruntime.Event{Type: agentruntime.EventAssistantTextDelta, Delta: base})
				model.refreshTranscript(true)
				b.StartTimer()
				for range 8 {
					model.Update(delta)
					_ = model.View().Content
				}
			}
		})
	}
}
