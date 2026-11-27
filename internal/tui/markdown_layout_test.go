package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
)

func TestMarkdownReadingLayout(t *testing.T) {
	model := newTestModel(t)
	const prose = "## 2. Recorder 如何进入 Context\n\nCLI 启动聊天时调用：\n\n"
	const code = "```go\nstartCall()\n\n    attachScope()\nendCall()\n```\n\n"
	text := prose + code + strings.Repeat("超宽段落", 50)
	check := func() {
		t.Helper()
		rows := strings.Split(model.View().Content, "\n")
		start := -1
		for i, row := range rows {
			if strings.Contains(ansi.Strip(row), "2. Recorder 如何进入 Context") {
				start = i
				break
			}
		}
		want := []string{"2. Recorder 如何进入 Context", "", "CLI 启动聊天时调用：", "", "startCall()", "", "attachScope()", "endCall()"}
		if start < 0 || start+len(want) > len(rows) {
			t.Fatalf("missing Markdown section in terminal frame: %q", rows)
		}
		for i, expected := range want {
			row := rows[start+i]
			got := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(ansi.Strip(row)), "│"))
			if got != expected || strings.Contains(row, codeBackground) != (i >= 4) {
				t.Fatalf("terminal row %d = %q, want %q (code=%t)", i, row, expected, i >= 4)
			}
		}
	}
	model.resize(95, 42)
	model.Update(runtimeEventMsg{event: agentruntime.Event{Type: agentruntime.EventAssistantTextDelta, Delta: text}})
	check()
	model.Update(runtimeEventMsg{event: agentruntime.Event{Type: agentruntime.EventAssistantMessage, Message: schema.AssistantMessage(text, nil)}})
	check()
	model.blocks = blocksFromHistory([]*schema.Message{schema.AssistantMessage(text, nil)})
	model.resize(56, 50)
	check()
}

func TestHeadingAndCodeWhileStreaming(t *testing.T) {
	model := newTestModel(t)
	send := func(delta string) {
		model.Update(runtimeEventMsg{event: agentruntime.Event{Type: agentruntime.EventAssistantTextDelta, Delta: delta}})
	}
	send("## A live ")
	send("heading")
	if plain := ansi.Strip(model.transcriptContent); !strings.Contains(plain, "A live heading") || strings.Contains(plain, "##") {
		t.Fatalf("live heading still shows syntax: %q", plain)
	}
	send("\n\n```sh\n# use a literal comment\n\n")
	send("echo READY")
	for _, width := range []int{56, 90} {
		model.resize(width, 30)
		rows := strings.Split(model.transcriptContent, "\n")
		n := len(rows)
		if n < 3 || !strings.Contains(ansi.Strip(rows[n-3]), "# use a literal comment") ||
			strings.TrimSpace(ansi.Strip(rows[n-2])) != "" || !strings.Contains(ansi.Strip(rows[n-1]), "echo READY") ||
			strings.Contains(model.transcriptContent, "```") {
			t.Fatalf("incomplete fence lost code, indentation or blank line: %q", rows)
		}
		for _, row := range rows[n-3:] {
			if !strings.Contains(row, codeBackground) {
				t.Fatalf("open code fence lost its background: %q", row)
			}
		}
	}
	send("\n```\n\n#hashtag and C#")
	if plain := ansi.Strip(model.transcriptContent); !strings.Contains(plain, "#hashtag and C#") || !strings.Contains(plain, "# use a literal comment") {
		t.Fatalf("literal hashes became heading syntax: %q", plain)
	}
}

func TestMarkdownHeadingLevelsAndBlankCodeRows(t *testing.T) {
	var renderer markdownRenderer
	for level := 1; level <= 6; level++ {
		heading := renderer.render(strings.Repeat("#", level)+" Heading with C#", 72)
		if got := strings.TrimSpace(ansi.Strip(heading)); got != "Heading with C#" || heading == ansi.Strip(heading) {
			t.Fatalf("H%d missing styled heading text: %q", level, heading)
		}
	}
	output := renderer.render("```go\n\n// keep the blank lines\n\n```", 72)
	rows := strings.Split(output, "\n")
	if len(rows) != 3 {
		t.Fatalf("three source rows became %d screen rows: %q", len(rows), output)
	}
	for _, row := range rows {
		if ansi.StringWidth(row) != 72 || !strings.Contains(row, codeBackground) {
			t.Fatalf("code background does not span the row: %q", row)
		}
	}
}
