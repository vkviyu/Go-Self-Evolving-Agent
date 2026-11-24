package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

func TestSessionTitleUsesFirstHumanQuestionAndExplicitName(t *testing.T) {
	history := []*schema.Message{
		schema.UserMessage(agentruntime.BackgroundInputHeader + "\nchild result"),
		schema.UserMessage("修复登录问题\n保留现有权限"),
		schema.AssistantMessage("done", nil),
		schema.UserMessage("second question"),
	}
	for _, test := range []struct{ name, title string }{
		{"", "修复登录问题 保留现有权限"},
		{"账户系统维护", "账户系统维护"},
	} {
		model, err := NewModel(context.Background(), Config{Conversation: &fakeConversation{history: history}, SessionTitle: test.name})
		if err != nil {
			t.Fatal(err)
		}
		if got := model.displaySessionTitle(); got != test.title {
			t.Fatalf("title = %q, want %q", got, test.title)
		}
		model.resize(80, 24)
		model.submitInputForTest("third question")
		if got := model.displaySessionTitle(); got != test.title {
			t.Fatalf("later question changed title to %q", got)
		}
	}
	model := newTestModel(t)
	model.submitInputForTest("首个问题 🙂\n还有一行")
	if got := model.displaySessionTitle(); got != "首个问题 🙂 还有一行" {
		t.Fatalf("first submitted question did not supply the title: %q", got)
	}
}

func TestSessionInfoMouseAndKeyboardPreserveDraftAndFrame(t *testing.T) {
	model := newTestModel(t)
	model.sessionTitle = "标题 🙂"
	model.workspace = "/tmp/private-workspace"
	model.sessionID = "session-detail"
	model.modelName = "model-detail"
	model.input.SetValue("unfinished draft 你好")
	model.resizeInput()
	collapsedHeight := model.viewport.Height
	if header := ansi.Strip(model.renderHeader(98)); strings.Contains(header, "private-workspace") || strings.Contains(header, "session-detail") || !strings.Contains(header, "Alt+I") {
		t.Fatalf("unexpected compact header: %q", header)
	}
	model.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, Y: 0, X: 2})
	if !model.showSessionInfo || model.viewport.Height != collapsedHeight-3 {
		t.Fatalf("info did not expand: height=%d", model.viewport.Height)
	}
	if header := ansi.Strip(model.renderHeader(98)); !strings.Contains(header, "private-workspace") || !strings.Contains(header, "session-detail") || !strings.Contains(header, "model-detail") {
		t.Fatalf("expanded info missing metadata: %q", header)
	}
	model.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'i'}, Alt: true})
	if model.showSessionInfo || model.viewport.Height != collapsedHeight {
		t.Fatal("Alt+I did not restore compact header")
	}
	if model.input.Value() != "unfinished draft 你好" || model.busy {
		t.Fatal("metadata toggle changed or submitted the draft")
	}
	for _, dimensions := range [][2]int{{80, 24}, {40, 12}, {24, 8}} {
		for _, expanded := range []bool{false, true} {
			model.showSessionInfo = expanded
			model.showActivity = true
			model.activity.Agents = []orchestration.AgentSnapshot{{ID: "agent-child", State: orchestration.AgentRunning}}
			model.input.SetValue(strings.Repeat("draft ", 20))
			model.resize(dimensions[0], dimensions[1])
			if got := lipgloss.Height(model.View()); got != dimensions[1] {
				t.Fatalf("%v expanded=%v: frame height=%d", dimensions, expanded, got)
			}
			if model.inputTopRow()+model.input.Height() != dimensions[1] {
				t.Fatalf("input position does not match final row: top=%d height=%d", model.inputTopRow(), model.input.Height())
			}
		}
	}
}

func TestQuestionNavigationTracksReadingAndJumpsWithoutSubmitting(t *testing.T) {
	model := navigationFixture(t)
	first, second := model.queryAnchors[0], model.queryAnchors[1]
	model.autoScroll = false
	model.viewport.SetYOffset(first.end + 2)
	if nav := ansi.Strip(model.renderQuestionNavigation(model.viewport.Width)); !strings.Contains(nav, "第一个问题") || !strings.Contains(nav, "Alt+P") {
		t.Fatalf("first answer has wrong query navigation: %q", nav)
	}
	model.input.SetValue("keep this draft")
	model.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: 3, Y: model.headerHeight()})
	if model.viewport.YOffset != first.start {
		t.Fatal("query navigation click did not jump to first question")
	}
	if nav := model.renderQuestionNavigation(model.viewport.Width); strings.Contains(ansi.Strip(nav), "Alt+P") {
		t.Fatal("already visible question was repeated in navigation")
	}
	model.viewport.SetYOffset(second.end + 2)
	if nav := ansi.Strip(model.renderQuestionNavigation(model.viewport.Width)); !strings.Contains(nav, "第二个问题") {
		t.Fatalf("second answer has wrong query navigation: %q", nav)
	}
	model.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}, Alt: true})
	if model.viewport.YOffset != second.start || model.autoScroll {
		t.Fatal("Alt+P did not jump to second question with manual scroll")
	}
	if model.input.Value() != "keep this draft" || model.busy {
		t.Fatal("query navigation submitted or changed the draft")
	}
	model.viewport.SetYOffset(second.end + 2)
	model.handleCommand("/question")
	if model.viewport.YOffset != second.start {
		t.Fatal("/question did not jump to the visible answer's question")
	}
}

func TestReadingQuestionSurvivesResizeAndToolDisclosure(t *testing.T) {
	model := navigationFixture(t)
	model.autoScroll = false
	model.viewport.SetYOffset(model.queryAnchors[1].end + 2)
	model.resize(48, 24)
	if query, ok := model.currentQuestion(); !ok || !strings.Contains(query.preview, "第二个问题") {
		t.Fatalf("resize changed reading question: %+v", query)
	}
	model.toggleToolDetails()
	model.closeToolInspector()
	if query, ok := model.currentQuestion(); !ok || !strings.Contains(query.preview, "第二个问题") {
		t.Fatalf("tool expansion changed reading question: %+v", query)
	}
	model.toggleSessionInfo()
	if query, ok := model.currentQuestion(); !ok || !strings.Contains(query.preview, "第二个问题") {
		t.Fatalf("info expansion changed reading question: %+v", query)
	}
	model.showActivity = true
	model.activity.Agents = []orchestration.AgentSnapshot{{ID: "agent-child", State: orchestration.AgentRunning}}
	model.resizeInput()
	line, inside := model.transcriptLine(model.transcriptTopRow())
	if !inside || line != model.viewport.YOffset {
		t.Fatalf("activity panel offset broke transcript selection: line=%d inside=%v", line, inside)
	}
	if _, inside := model.transcriptLine(model.transcriptTopRow() - 1); inside {
		t.Fatal("activity panel was treated as transcript")
	}
}

func TestViewerUsesItsOwnQuestionsWithoutChangingSessionTitle(t *testing.T) {
	model := navigationFixture(t)
	title := model.displaySessionTitle()
	model.transcripts = fakeTranscriptReader{sessions: map[session.ID][]*schema.Message{"child-session": {
		schema.UserMessage("child-specific question"), schema.AssistantMessage(strings.Repeat("child answer\n", 60), nil),
	}}}
	runAction(t, model, model.openAgentTranscript(orchestration.AgentSnapshot{ID: "child", ParentAgentID: "root", SessionID: "child-session"}))
	if query, ok := model.currentQuestion(); !ok || query.preview != "child-specific question" {
		t.Fatalf("viewer retained root query navigation: %+v", query)
	}
	if model.displaySessionTitle() != title {
		t.Fatal("viewing a child changed the root session title")
	}
	model.closeTranscriptViewer()
	if query, ok := model.currentQuestion(); !ok || !strings.Contains(query.preview, "第二个问题") {
		t.Fatalf("returning from viewer retained child query: %+v", query)
	}
}

func TestAssistantTerminalMessagesReconcileStreamedPreview(t *testing.T) {
	model := newTestModel(t)
	model.busy = true
	model.applyRuntimeEvent(agentruntime.Event{Type: agentruntime.EventAssistantTextDelta, Delta: "tool prelude"})
	model.applyAssistantMessage(schema.AssistantMessage("tool prelude complete", []schema.ToolCall{{ID: "call", Function: schema.FunctionCall{Name: "workspace_read", Arguments: `{}`}}}))
	model.applyToolResult(&schema.Message{Role: schema.Tool, ToolName: "workspace_read", ToolCallID: "call", Content: `{"content":"file"}`})
	model.applyRuntimeEvent(agentruntime.Event{Type: agentruntime.EventAssistantReasoningDelta, Delta: "reasoning"})
	model.applyRuntimeEvent(agentruntime.Event{Type: agentruntime.EventAssistantTextDelta, Delta: "越强的技术门槛，"})
	model.applyAssistantMessage(schema.AssistantMessage("越强的技术门槛，或者越不可持续的代币补贴。", nil))
	model.finishTurn(turnDoneMsg{result: agentruntime.Result{Final: schema.AssistantMessage("越强的技术门槛，或者越不可持续的代币补贴。以上是一般性科普。", nil)}})
	var assistants []string
	for _, block := range model.blocks {
		if block.kind == blockAssistant {
			assistants = append(assistants, block.content)
		}
	}
	if len(assistants) != 2 || assistants[0] != "tool prelude complete" || assistants[1] != "越强的技术门槛，或者越不可持续的代币补贴。以上是一般性科普。" {
		t.Fatalf("canonical messages were lost, duplicated or mixed across tools: %+v", assistants)
	}
	if plain := compactDisplay(model.View()); !strings.Contains(plain, "或者越不可持续的代币补贴。以上是一般性科普。") {
		t.Fatalf("canonical suffix missing from visible final frame: %s", plain)
	}
}

func TestTurnCompletionPreservesHistoricalReadingPosition(t *testing.T) {
	model := navigationFixture(t)
	model.busy = true
	model.autoScroll = false
	model.viewport.SetYOffset(model.queryAnchors[0].end + 3)
	before := model.viewport.YOffset
	model.applyRuntimeEvent(agentruntime.Event{Type: agentruntime.EventAssistantTextDelta, Delta: "new streamed answer"})
	model.refreshTranscript(false)
	model.finishTurn(turnDoneMsg{result: agentruntime.Result{Final: schema.AssistantMessage("new streamed answer complete", nil)}})
	if model.viewport.YOffset != before || model.autoScroll {
		t.Fatal("completion pulled the reader away from history")
	}
	if query, ok := model.currentQuestion(); !ok || !strings.Contains(query.preview, "第一个问题") {
		t.Fatalf("completion changed the historical question: %+v", query)
	}
}

func TestUnspacedCJKAndUnicodeTailRemainsVisible(t *testing.T) {
	const tail = "回答尾部完整标记"
	answer := strings.Repeat("这是无空格中文说明，🙂以及技术门槛。", 60) + tail
	for _, width := range []int{24, 40, 80, 120, 186, 240} {
		for _, height := range []int{24, 50} {
			t.Run(fmt.Sprintf("%dx%d", width, height), func(t *testing.T) {
				model, err := NewModel(context.Background(), Config{Conversation: &fakeConversation{history: []*schema.Message{schema.UserMessage("中文测试"), schema.AssistantMessage(answer, nil)}}})
				if err != nil {
					t.Fatal(err)
				}
				model.resize(width, height)
				if !strings.Contains(compactDisplay(model.View()), tail) {
					t.Fatalf("visible tail was clipped at %d columns", width)
				}
				for line := range strings.SplitSeq(model.transcriptContent, "\n") {
					if got := ansi.StringWidth(line); got > model.viewport.Width {
						t.Fatalf("rendered line width=%d exceeds viewport %d", got, model.viewport.Width)
					}
				}
				if got := lipgloss.Height(model.View()); got != height {
					t.Fatalf("frame height=%d, want %d", got, height)
				}
			})
		}
	}
}

func navigationFixture(t *testing.T) *Model {
	t.Helper()
	model := newTestModel(t)
	model.blocks = []transcriptBlock{
		{kind: blockUser, content: "第一个问题 🙂\n跨行补充"},
		{kind: blockAssistant, content: strings.Repeat("第一个回答内容\n", 60)},
		{kind: blockTool, toolName: "read", toolResult: strings.Repeat("tool result\n", 20)},
		{kind: blockTool, toolName: "list", toolResult: strings.Repeat("tool result\n", 20)},
		{kind: blockUser, content: "第二个问题 👩‍💻"},
		{kind: blockAssistant, content: strings.Repeat("第二个回答内容\n", 60)},
	}
	model.resize(80, 24)
	return model
}

func compactDisplay(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, ansi.Strip(value))
}
