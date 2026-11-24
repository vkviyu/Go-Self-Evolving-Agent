package tui

import (
	"bytes"
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/cloudwego/eino/schema"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/approval"
	"github.com/vkviyu/go-self-evolving-agent/internal/policy"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenizer"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenusage"
)

type fakeConversation struct {
	id      session.ID
	history []*schema.Message
}

func (conversation *fakeConversation) ID() session.ID { return conversation.id }

func (conversation *fakeConversation) History() []*schema.Message {
	return append([]*schema.Message(nil), conversation.history...)
}

func (conversation *fakeConversation) CompletedTurns() int {
	count := 0
	for _, message := range conversation.history {
		if message != nil && message.Role == schema.User {
			count++
		}
	}
	return count
}

func (*fakeConversation) RunTurn(context.Context, string, agentruntime.EventSink) (agentruntime.Result, error) {
	return agentruntime.Result{}, nil
}

func TestModelRendersStableTerminalLayout(t *testing.T) {
	conversation := &fakeConversation{
		id: "session-1",
		history: []*schema.Message{
			schema.UserMessage("previous question"),
			schema.AssistantMessage("previous answer", nil),
		},
	}
	model, err := NewModel(context.Background(), Config{
		Conversation:    conversation,
		Model:           "gpt-test",
		Provider:        "openai",
		Workspace:       "/tmp/project",
		ReasoningEffort: "high",
	})
	if err != nil {
		t.Fatal(err)
	}
	model.resize(80, 24)
	view := model.View()
	plain := ansi.Strip(view)
	for _, expected := range []string{"GSEA", "previous question", "previous answer", "Ask GSEA", "Alt+I"} {
		if !strings.Contains(plain, expected) {
			t.Fatalf("view does not contain %q:\n%s", expected, plain)
		}
	}
	model.toggleSessionInfo()
	expanded := ansi.Strip(model.View())
	for _, expected := range []string{"openai/gpt-test", "workspace  /tmp/project", "session    session-1"} {
		if !strings.Contains(expanded, expected) {
			t.Fatalf("expanded info missing %q:\n%s", expected, expanded)
		}
	}
	model.toggleSessionInfo()
	if height := lipgloss.Height(view); height != 24 {
		t.Fatalf("view height = %d, want 24", height)
	}
	for line := range strings.SplitSeq(view, "\n") {
		if width := ansi.StringWidth(line); width > 80 {
			t.Fatalf("line width = %d, want <= 80: %q", width, ansi.Strip(line))
		}
	}
	lines := strings.Split(strings.TrimSuffix(plain, "\n"), "\n")
	if len(lines) == 0 || !strings.Contains(lines[len(lines)-1], "›") {
		t.Fatalf("input prompt is not on the final frame row: %q", lines)
	}
}

func TestModelRestoresLastUsageFromHistory(t *testing.T) {
	answer := schema.AssistantMessage("previous answer", nil)
	answer.ResponseMeta = &schema.ResponseMeta{Usage: &schema.TokenUsage{
		PromptTokens: 10,
		PromptTokenDetails: schema.PromptTokenDetails{
			CachedTokens: 8,
		},
		CompletionTokens: 4,
		TotalTokens:      14,
		CompletionTokensDetails: schema.CompletionTokensDetails{
			ReasoningTokens: 2,
		},
	}}
	model, err := NewModel(context.Background(), Config{
		Conversation: &fakeConversation{
			id:      "session-resumed",
			history: []*schema.Message{schema.UserMessage("previous question"), answer},
		},
		Model:    "gpt-test",
		Provider: "openai",
	})
	if err != nil {
		t.Fatal(err)
	}
	model.resize(100, 24)
	plain := ansi.Strip(model.View())
	for _, expected := range []string{"in 10", "out 4", "reasoning 2", "total 14", "cache 8 80.0%"} {
		if !strings.Contains(plain, expected) {
			t.Fatalf("resumed view does not contain %q:\n%s", expected, plain)
		}
	}
}

func TestInputHeightAccountsForSoftWrappedText(t *testing.T) {
	model := newTestModel(t)
	model.resize(40, 24)
	model.input.SetValue(strings.Repeat("x", model.input.Width()*2+1))
	model.resizeInput()

	if model.input.Height() < 3 {
		t.Fatalf("input height = %d, want at least 3 for soft-wrapped text", model.input.Height())
	}
	if model.viewport.Height != 24-model.headerHeight()-3-model.input.Height() {
		t.Fatalf("viewport height = %d, expected it to shrink for multi-line input", model.viewport.Height)
	}
	if got := lipgloss.Height(model.renderInput()); got != model.input.Height() {
		t.Fatalf("rendered input height = %d, want %d", got, model.input.Height())
	}
}

func TestInputWrapBoundaryKeepsAllRowsVisible(t *testing.T) {
	model := newTestModel(t)
	model.resize(40, 24)
	width := model.input.Width()
	wantHeights := map[int]int{
		width - 1:   1,
		width:       2,
		width + 1:   2,
		width * 2:   3,
		width*2 + 1: 3,
	}
	for _, length := range []int{width - 1, width, width + 1, width * 2, width*2 + 1} {
		model.input.SetValue(strings.Repeat("x", length))
		model.resizeInput()
		lines := strings.Split(ansi.Strip(model.renderInput()), "\n")
		wantHeight := wantHeights[length]
		if len(lines) != wantHeight {
			t.Fatalf("length=%d rendered lines=%d, want %d: %q", length, len(lines), wantHeight, ansi.Strip(model.renderInput()))
		}
		joined := strings.ReplaceAll(strings.Join(lines, ""), "›", "")
		if got := strings.Count(joined, "x"); got != length {
			t.Fatalf("length=%d rendered x count=%d: %q", length, got, joined)
		}
	}
}

func TestInputTypingExpandsPastTwoRows(t *testing.T) {
	model := newTestModel(t)
	model.resize(40, 24)
	width := model.input.Width()
	model.input.Focus()
	for index := 0; index < width*3+1; index++ {
		var command tea.Cmd
		model.input, command = model.input.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
		_ = command
		model.resizeInput()
		if model.input.Value() != strings.Repeat("x", index+1) {
			t.Fatalf("typed value at %d = %q", index+1, model.input.Value())
		}
	}
	if got := model.inputDisplayHeight(); got < 4 {
		t.Fatalf("input display height = %d, want at least 4", got)
	}
	if model.inputCursorDisplayLine() < model.inputVisibleStart(model.inputDisplayHeight()) {
		t.Fatalf("cursor row %d is hidden above offset %d", model.inputCursorDisplayLine(), model.inputVisibleStart(model.inputDisplayHeight()))
	}
}

func TestInputRenderPreservesWideAndSpacedText(t *testing.T) {
	model := newTestModel(t)
	model.resize(48, 24)
	value := "你好 世界 这是一个跨行输入测试"
	model.input.SetValue(value)
	model.resizeInput()
	rendered := model.renderInput()
	plain := ansi.Strip(rendered)
	if got := strings.Count(plain, "你") + strings.Count(plain, "好") + strings.Count(plain, "世") + strings.Count(plain, "界"); got != 4 {
		t.Fatalf("wide characters rendered=%d, want 4: %q", got, plain)
	}
	if !strings.Contains(plain, "这是一个跨行输入测试") {
		t.Fatalf("input text was truncated: %q", plain)
	}
	for index, line := range strings.Split(rendered, "\n") {
		if width := ansi.StringWidth(line); width != model.input.Width()+ansi.StringWidth(model.input.Prompt) {
			t.Fatalf("line %d width=%d, want %d: %q", index, width, model.input.Width()+ansi.StringWidth(model.input.Prompt), ansi.Strip(line))
		}
	}
}

func TestInputCtrlASelectsAllText(t *testing.T) {
	model := newTestModel(t)
	model.input.SetValue("第一行\nsecond line")
	if _, handled := model.handleKey(tea.KeyMsg{Type: tea.KeyCtrlA}); !handled {
		t.Fatal("Ctrl+A was not handled by the TUI")
	}
	if got, want := model.selectedInputText(), model.input.Value(); got != want {
		t.Fatalf("selected input = %q, want %q", got, want)
	}
	if !model.inputSelection.active {
		t.Fatal("Ctrl+A did not activate input selection")
	}
}

func TestInputAltASelectsAllText(t *testing.T) {
	model := newTestModel(t)
	model.input.SetValue("第一行\nsecond line")
	if _, handled := model.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}, Alt: true}); !handled {
		t.Fatal("Alt+A was not handled by the TUI")
	}
	if got, want := model.selectedInputText(), model.input.Value(); got != want {
		t.Fatalf("selected input = %q, want %q", got, want)
	}
}

func TestInputMouseDragSelectsText(t *testing.T) {
	model := newTestModel(t)
	model.resize(60, 24)
	model.input.SetValue("copy this input")
	model.resizeInput()
	top := model.inputTopRow()

	if _, handled := model.handleInputMouse(tea.MouseMsg{
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonLeft,
		X:      3,
		Y:      top,
	}); !handled {
		t.Fatal("input mouse press was not handled")
	}
	if _, handled := model.handleInputMouse(tea.MouseMsg{
		Action: tea.MouseActionMotion,
		Button: tea.MouseButtonLeft,
		X:      12,
		Y:      top,
	}); !handled {
		t.Fatal("input mouse motion was not handled")
	}
	if got, want := model.selectedInputText(), "copy this"; got != want {
		t.Fatalf("drag selection = %q, want %q", got, want)
	}
}

func TestInputMouseDragSelectsAcrossWrappedRows(t *testing.T) {
	model := newTestModel(t)
	model.resize(40, 24)
	model.input.SetValue(strings.Repeat("x", model.input.Width()+4))
	model.resizeInput()
	top := model.inputTopRow()
	if _, handled := model.handleInputMouse(tea.MouseMsg{
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonLeft,
		X:      3,
		Y:      top,
	}); !handled {
		t.Fatal("input mouse press was not handled")
	}
	if _, handled := model.handleInputMouse(tea.MouseMsg{
		Action: tea.MouseActionMotion,
		Button: tea.MouseButtonLeft,
		X:      8,
		Y:      top + 1,
	}); !handled {
		t.Fatal("input mouse motion was not handled")
	}
	selected := model.selectedInputText()
	if len([]rune(selected)) != model.input.Width()+4 {
		t.Fatalf("wrapped drag selection length=%d, want %d", len([]rune(selected)), model.input.Width()+4)
	}
}

func TestCursorWriterPlacesPhysicalCursorAfterPrompt(t *testing.T) {
	var output bytes.Buffer
	writer := cursorWriter{output: &output}
	frame := []byte("frame" + cursorMarker + "7\a\r")
	if _, err := writer.Write(frame); err != nil {
		t.Fatal(err)
	}
	if got, want := output.String(), "\rframe\x1b[8G"; got != want {
		t.Fatalf("cursor frame = %q, want %q", got, want)
	}
	output.Reset()
	frame = []byte("frame" + cursorMarker + "7\a\x1b[24;H")
	if _, err := writer.Write(frame); err != nil {
		t.Fatal(err)
	}
	if got, want := output.String(), "frame\x1b[24;8H"; got != want {
		t.Fatalf("alternate-screen cursor frame = %q, want %q", got, want)
	}
	output.Reset()
	frame = []byte("top\r\ninput" + cursorMarker + "2,7\a\r")
	if _, err := writer.Write(frame); err != nil {
		t.Fatal(err)
	}
	if got, want := output.String(), "\rtop\r\ninput\x1b[2A\x1b[8G"; got != want {
		t.Fatalf("multiline cursor frame = %q, want %q", got, want)
	}
}

func TestHistoryGroupsToolCallWithResult(t *testing.T) {
	blocks := blocksFromHistory([]*schema.Message{
		schema.AssistantMessage("", []schema.ToolCall{{
			ID: "call-1",
			Function: schema.FunctionCall{
				Name:      "workspace_write",
				Arguments: `{"path":"approval-demo.txt","content":"ok"}`,
			},
		}}),
		schema.ToolMessage(`{"bytes_written":2}`, "call-1", schema.WithToolName("workspace_write")),
	})
	if len(blocks) != 1 {
		t.Fatalf("blocks = %#v", blocks)
	}
	if !strings.Contains(blocks[0].content, "workspace_write") || !strings.Contains(blocks[0].toolResult, "bytes_written") {
		t.Fatalf("grouped tool block = %#v", blocks[0])
	}
	model := newTestModel(t)
	rendered := ansi.Strip(model.renderBlock(&blocks[0]))
	if !strings.Contains(rendered, "workspace_write  ▸") {
		t.Fatalf("collapsed tool block is missing its header: %q", rendered)
	}
	if strings.Contains(rendered, "arguments") || strings.Contains(rendered, "result") {
		t.Fatalf("collapsed tool block leaked detail labels: %q", rendered)
	}
	blocks[0].argumentsExpanded = true
	blocks[0].resultExpanded = true
	rendered = ansi.Strip(model.renderBlock(&blocks[0]))
	for _, label := range []string{"arguments", "result"} {
		if !strings.Contains(rendered, label) {
			t.Fatalf("expanded tool block is missing %q: %q", label, rendered)
		}
	}
}

func TestToolResultRendersUnifiedDiffSeparately(t *testing.T) {
	model := newTestModel(t)
	block := transcriptBlock{
		kind:           blockTool,
		content:        "workspace_write",
		toolName:       "workspace_write",
		toolResult:     `{"path":"main.go","diff":{"path":"main.go","kind":"modified","status":"ready","content":"--- a/main.go\n+++ b/main.go\n@@ -1 +1 @@\n-old\n+new\n"}}`,
		resultExpanded: true,
	}
	rendered := ansi.Strip(model.renderBlock(&block))
	for _, expected := range []string{"diff main.go [ready]", "-old", "+new"} {
		if !strings.Contains(rendered, expected) {
			t.Fatalf("rendered tool result missing %q: %q", expected, rendered)
		}
	}
	if strings.Contains(rendered, `"content":`) {
		t.Fatalf("diff content was rendered as escaped JSON: %q", rendered)
	}
}

func TestDiffCommandInspectsDiffResults(t *testing.T) {
	model := newTestModel(t)
	model.blocks = []transcriptBlock{{
		kind:       blockTool,
		content:    "workspace_edit",
		toolName:   "workspace_edit",
		toolResult: `{"diff":{"path":"main.go","status":"ready","content":"+new\n"}}`,
	}}
	if _, handled := model.handleCommand("/diff"); !handled {
		t.Fatal("/diff command was not handled")
	}
	if model.toolInspector == nil || !strings.Contains(model.transcriptContent, "+new") {
		t.Fatal("/diff did not show the diff result")
	}
}

func TestMultipleToolCallsRemainVisibleWhenCollapsed(t *testing.T) {
	model := newTestModel(t)
	model.applyAssistantMessage(schema.AssistantMessage("", []schema.ToolCall{
		{ID: "call-1", Function: schema.FunctionCall{Name: "runtime_diagnostics", Arguments: `{}`}},
		{ID: "call-2", Function: schema.FunctionCall{Name: "workspace_list", Arguments: `{"path":"."}`}},
		{ID: "call-3", Function: schema.FunctionCall{Name: "workspace_write", Arguments: `{"path":"test.txt","content":"ok"}`}},
	}))
	model.refreshTranscript(true)
	plain := ansi.Strip(model.transcriptContent)
	for _, expected := range []string{"runtime_diagnostics", "workspace_list", "workspace_write"} {
		if !strings.Contains(plain, expected) {
			t.Fatalf("collapsed transcript lost tool %q: %q", expected, plain)
		}
	}
	for _, hidden := range []string{"arguments", "result", `"path":"test.txt"`} {
		if strings.Contains(plain, hidden) {
			t.Fatalf("collapsed transcript leaked tool detail %q: %q", hidden, plain)
		}
	}
}

func TestToolBlockRendersDistinctSections(t *testing.T) {
	model := newTestModel(t)
	block := transcriptBlock{
		kind:              blockTool,
		content:           "workspace_write",
		toolName:          "workspace_write",
		toolArguments:     `{"path":"approval-check.txt","content":"approval test"}`,
		approvalReason:    "writing files changes the workspace",
		toolResult:        `{"bytes_written":14,"path":"approval-check.txt"}`,
		approvalState:     approvalApproved,
		argumentsExpanded: true,
		resultExpanded:    true,
	}
	rendered := ansi.Strip(model.renderBlock(&block))
	for _, expected := range []string{
		"workspace_write",
		"arguments",
		"approval reason",
		"result",
		"✓",
	} {
		if !strings.Contains(rendered, expected) {
			t.Fatalf("tool block is missing %q: %q", expected, rendered)
		}
	}
	for index, line := range strings.Split(model.renderBlock(&block), "\n") {
		if width := ansi.StringWidth(line); width > model.viewport.Width-1 {
			t.Fatalf("tool line %d width=%d exceeds %d: %q", index, width, model.viewport.Width-1, ansi.Strip(line))
		}
	}
}

func TestToolDetailsDefaultCollapsedAndToggleWithMouse(t *testing.T) {
	model := newTestModel(t)
	model.blocks = []transcriptBlock{{
		kind:          blockTool,
		content:       "workspace_write",
		toolName:      "workspace_write",
		toolArguments: `{"path":"approval-check.txt"}`,
		toolResult:    `{"bytes_written":14}`,
		approvalState: approvalApproved,
	}}
	model.refreshTranscript(true)
	plain := ansi.Strip(model.transcriptContent)
	if !strings.Contains(plain, "workspace_write  ▸") {
		t.Fatalf("collapsed tool header is not rendered: %q", plain)
	}
	if strings.Contains(plain, "arguments") || strings.Contains(plain, "result") {
		t.Fatalf("collapsed tool detail labels leaked: %q", plain)
	}
	if strings.Contains(plain, "approval-check.txt") || strings.Contains(plain, "bytes_written") {
		t.Fatalf("collapsed tool details leaked payload: %q", plain)
	}
	lines := strings.Split(model.transcriptContent, "\n")
	headerRow := -1
	for index, line := range lines {
		if strings.Contains(ansi.Strip(line), "workspace_write  ▸") {
			headerRow = index
			break
		}
	}
	if headerRow < 0 {
		t.Fatal("collapsed tool header was not rendered")
	}
	if _, handled := model.handleTranscriptMouse(tea.MouseMsg{
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonLeft,
		X:      5,
		Y:      model.transcriptTopRow() + headerRow,
	}); !handled {
		t.Fatal("tool header click was not handled")
	}
	model.refreshTranscript(true)
	if model.toolInspector == nil || !model.toolInspector.detail {
		t.Fatal("tool details did not open")
	}
	if !strings.Contains(ansi.Strip(model.transcriptContent), "approval-check.txt") {
		t.Fatal("expanded arguments are not visible")
	}
}

func TestToolDetailsKeyboardToggleReturnsToChat(t *testing.T) {
	model := newTestModel(t)
	model.blocks = []transcriptBlock{{
		kind:          blockTool,
		content:       "workspace_write",
		toolName:      "workspace_write",
		toolArguments: `{"path":"approval-check.txt"}`,
		toolResult:    `{"bytes_written":14}`,
	}}
	if _, handled := model.handleKey(tea.KeyMsg{Type: tea.KeyCtrlO}); !handled {
		t.Fatal("Ctrl+O was not handled")
	}
	if model.toolInspector == nil || !strings.Contains(model.transcriptContent, "bytes_written") {
		t.Fatal("Ctrl+O did not open tool details")
	}
	if _, handled := model.handleKey(tea.KeyMsg{Type: tea.KeyCtrlO}); !handled {
		t.Fatal("second Ctrl+O was not handled")
	}
	if model.toolInspector != nil || strings.Contains(model.transcriptContent, "bytes_written") {
		t.Fatal("second Ctrl+O did not return to chat")
	}
}

func TestApprovalReasonRendersInlineAndWrapsLongText(t *testing.T) {
	model := newTestModel(t)
	model.resize(60, 24)
	block := transcriptBlock{
		kind:           blockTool,
		content:        "workspace_write",
		toolName:       "workspace_write",
		approvalReason: "writing files changes the workspace",
	}
	short := ansi.Strip(model.renderBlock(&block))
	if !strings.Contains(short, "? approval reason: writing files changes the workspace") {
		t.Fatalf("short approval reason is not inline: %q", short)
	}
	block.approvalReason = strings.Repeat("reason ", 30)
	long := model.renderBlock(&block)
	plain := ansi.Strip(long)
	if !strings.Contains(plain, "? approval reason: reason") {
		t.Fatalf("long approval reason lost its inline prefix: %q", plain)
	}
	if strings.Count(plain, "reason") < 30 {
		t.Fatalf("long approval reason was truncated: %q", plain)
	}
	for index, line := range strings.Split(long, "\n") {
		if width := ansi.StringWidth(line); width > model.viewport.Width-1 {
			t.Fatalf("approval reason line %d width=%d exceeds %d: %q", index, width, model.viewport.Width-1, ansi.Strip(line))
		}
	}
}

func TestApprovalAugmentsExistingToolBlock(t *testing.T) {
	model := newTestModel(t)
	model.applyAssistantMessage(schema.AssistantMessage("", []schema.ToolCall{{
		ID: "call-1",
		Function: schema.FunctionCall{
			Name:      "workspace_write",
			Arguments: `{"path":"approval-check.txt","content":"approval test"}`,
		},
	}}))
	model.applyApprovalRequest(approval.Request{
		ID: "call-1",
		Policy: policy.Request{
			CallID:        "call-1",
			ToolName:      "workspace_write",
			NormalizedArg: []byte(`{"path":"approval-check.txt","content":"approval test"}`),
		},
		Decision: policy.Decision{Reason: "writing files changes the workspace"},
	})
	if len(model.blocks) != 1 {
		t.Fatalf("approval created a duplicate block: %#v", model.blocks)
	}
	if count := strings.Count(model.blocks[0].content, "workspace_write"); count != 1 {
		t.Fatalf("tool name appears %d times, want once: %q", count, model.blocks[0].content)
	}
	if model.blocks[0].approvalReason != "writing files changes the workspace" {
		t.Fatalf("approval reason missing: %q", model.blocks[0].approvalReason)
	}
	pendingRendered := ansi.Strip(model.renderBlock(&model.blocks[0]))
	if !strings.Contains(pendingRendered, "?") || !strings.Contains(pendingRendered, "Allow") || !strings.Contains(pendingRendered, "Reject") {
		t.Fatalf("pending approval rendering missing: %q", pendingRendered)
	}
	if strings.Contains(pendingRendered, "status: approval required") {
		t.Fatalf("tool card repeats the status bar's pending label: %q", pendingRendered)
	}
	model.resolveApproval(true)
	if model.blocks[0].approvalState != approvalApproved {
		t.Fatalf("approval state = %d, want approved", model.blocks[0].approvalState)
	}
	if rendered := ansi.Strip(model.renderBlock(&model.blocks[0])); strings.Contains(rendered, "approval: approved") || !strings.Contains(rendered, "✓") {
		t.Fatalf("approval result rendering is too verbose or missing marker: %q", rendered)
	}
}

func TestMergeApprovalShowsThePlanDiff(t *testing.T) {
	model := newTestModel(t)
	model.applyApprovalRequest(approval.Request{
		ID: "approval-1",
		Policy: policy.Request{CallID: "merge-1", ToolName: "merge_apply", NormalizedArg: []byte(
			`{"merge_id":"merge-1","paths":["a.txt"],"diff":{"status":"planned","content":"--- a/a.txt\n+++ b/a.txt\n-base\n+changed"}}`,
		)},
		Source: approval.Source{AgentID: "agent_0123456789abcdef0123456789abcdef", Profile: "worker", Task: "Change a"},
	})
	rendered := ansi.Strip(model.renderBlock(&model.blocks[0]))
	if !strings.Contains(rendered, "▾ diff [planned]") || !strings.Contains(rendered, "+changed") || strings.Contains(rendered, `"content"`) {
		t.Fatalf("merge approval does not show its plan as a diff: %q", rendered)
	}
}

func TestDelegatedApprovalsQueueAndShowTheirSource(t *testing.T) {
	model := newTestModel(t)
	model.applyAssistantMessage(schema.AssistantMessage("", []schema.ToolCall{{
		ID:       "call_0",
		Function: schema.FunctionCall{Name: "workspace_read", Arguments: `{"path":"README.md"}`},
	}}))
	child := approval.Request{
		ID:     "approval-1",
		Policy: policy.Request{CallID: "call_0", ToolName: "workspace_write", NormalizedArg: []byte(`{"path":"notes.txt"}`)},
		Source: approval.Source{AgentID: "agent_0123456789abcdef0123456789abcdef", Profile: "worker", Task: "Collect notes"},
	}
	root := approval.Request{ID: "approval-2", Policy: policy.Request{CallID: "call_1", ToolName: "shell_exec"}}
	model.applyApprovalRequest(child)
	model.applyApprovalRequest(root)
	if len(model.blocks) != 2 || model.blocks[0].approvalState != approvalNone {
		t.Fatalf("a child request reusing the root call ID attached to the root tool card: %#v", model.blocks)
	}
	if model.approvalPending == nil || model.approvalPending.ID != child.ID || len(model.approvalQueue) != 1 {
		t.Fatalf("second request did not wait behind the first: pending=%v queue=%v", model.approvalPending, model.approvalQueue)
	}
	rendered := ansi.Strip(model.renderBlock(&model.blocks[1]))
	if !strings.Contains(rendered, "requested by worker agent_01...cdef · Collect notes") {
		t.Fatalf("child approval does not name its source: %q", rendered)
	}
	model.resolveApproval(false)
	if model.approvalPending == nil || model.approvalPending.ID != root.ID || len(model.approvalQueue) != 0 {
		t.Fatalf("queued request was not shown after the first decision: pending=%v queue=%v", model.approvalPending, model.approvalQueue)
	}
	if model.blocks[1].approvalState != approvalRejected || len(model.blocks) != 3 || model.blocks[2].approvalSource != "" {
		t.Fatalf("decision or root card state is wrong: %#v", model.blocks)
	}
	model.resolveApproval(true)
	if model.approvalPending != nil || model.stage != "Ready" {
		t.Fatalf("idle model after the last decision: pending=%v stage=%q", model.approvalPending, model.stage)
	}
}

func TestActiveApprovalStatusAppearsOnceInView(t *testing.T) {
	for _, attached := range []bool{false, true} {
		for _, approved := range []bool{false, true} {
			model := newTestModel(t)
			model.resize(120, 30)
			if attached {
				model.applyAssistantMessage(schema.AssistantMessage("", []schema.ToolCall{{
					ID: "shell-1", Function: schema.FunctionCall{Name: "shell_exec", Arguments: `{"command":"pwd"}`},
				}}))
			}
			model.applyApprovalRequest(approval.Request{
				ID: "shell-1", Policy: policy.Request{CallID: "shell-1", ToolName: "shell_exec", NormalizedArg: []byte(`{"command":"pwd"}`)},
				Decision: policy.Decision{Reason: "shell commands can modify the system"},
			})
			model.refreshTranscript(true)
			plain := ansi.Strip(model.View())
			if count := strings.Count(plain, "approval required"); count != 1 {
				t.Fatalf("attached=%v: pending label appears %d times:\n%s", attached, count, plain)
			}
			for _, expected := range []string{"approval reason: shell commands can modify the system", "Allow", "Reject", "↑↓ select", "Enter confirm", "Esc reject"} {
				if !strings.Contains(plain, expected) {
					t.Fatalf("attached=%v: missing %q:\n%s", attached, expected, plain)
				}
			}
			model.resolveApproval(approved)
			plain = ansi.Strip(model.View())
			if approved {
				if strings.Contains(plain, "approval: approved") || !strings.Contains(plain, "✓") {
					t.Fatalf("attached=%v approved=%v: successful approval was too verbose:\n%s", attached, approved, plain)
				}
			} else if !strings.Contains(plain, "approval: rejected") {
				t.Fatalf("attached=%v approved=%v: rejection status missing:\n%s", attached, approved, plain)
			}
			if strings.Contains(plain, "approval required") || strings.Contains(plain, "Enter confirm") {
				t.Fatalf("attached=%v approved=%v: incorrect resolved view:\n%s", attached, approved, plain)
			}
		}
	}
}

func TestApprovalUsesArrowSelectionAndEnter(t *testing.T) {
	model := newTestModel(t)
	model.applyApprovalRequest(approval.Request{
		ID:       "call-1",
		Policy:   policy.Request{CallID: "call-1", ToolName: "workspace_write"},
		Decision: policy.Decision{Reason: "writing files changes the workspace"},
	})
	if model.approvalChoice != approvalAllow {
		t.Fatalf("initial approval choice = %d, want allow", model.approvalChoice)
	}
	if rendered := ansi.Strip(model.renderBlock(&model.blocks[0])); !strings.Contains(rendered, "› Allow") || strings.Contains(rendered, "› Reject") {
		t.Fatalf("initial approval options = %q", rendered)
	}
	if _, handled := model.handleKey(tea.KeyMsg{Type: tea.KeyDown}); !handled {
		t.Fatal("down arrow was not handled")
	}
	if model.approvalChoice != approvalReject {
		t.Fatalf("after down approval choice = %d, want reject", model.approvalChoice)
	}
	if rendered := ansi.Strip(model.renderBlock(&model.blocks[0])); !strings.Contains(rendered, "› Reject") || strings.Contains(rendered, "› Allow") {
		t.Fatalf("selected reject options = %q", rendered)
	}
	if _, handled := model.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}}); !handled || model.approvalPending == nil {
		t.Fatal("y unexpectedly resolved approval")
	}
	if _, handled := model.handleKey(tea.KeyMsg{Type: tea.KeyEnter}); !handled {
		t.Fatal("enter was not handled")
	}
	if model.approvalPending != nil || model.blocks[0].approvalState != approvalRejected {
		t.Fatalf("approval was not rejected through selected option: pending=%v state=%d", model.approvalPending != nil, model.blocks[0].approvalState)
	}
}

func TestTranscriptSelectionUsesRenderedLines(t *testing.T) {
	model := newTestModel(t)
	model.blocks = []transcriptBlock{
		{kind: blockUser, content: "copy this prompt"},
		{kind: blockAssistant, content: "copy this answer"},
	}
	model.refreshTranscript(true)
	model.selection = transcriptSelection{active: true, start: 0, end: 2, startColumn: 0, endColumn: 999}
	selected, lines := model.selectedTranscript()
	if lines != 3 {
		t.Fatalf("selected lines = %d, want 3", lines)
	}
	for _, expected := range []string{"copy this prompt", "copy this answer"} {
		if !strings.Contains(selected, expected) {
			t.Fatalf("selected transcript missing %q: %q", expected, selected)
		}
	}
	rendered := ansi.Strip(model.renderViewport())
	if !strings.Contains(rendered, "copy this prompt") {
		t.Fatalf("selection disappeared from viewport: %q", rendered)
	}
}

func TestTranscriptUsesDistinctUserReasoningAndAssistantMarkers(t *testing.T) {
	model := newTestModel(t)
	model.blocks = []transcriptBlock{
		{kind: blockUser, content: "user question"},
		{kind: blockReasoning, content: "internal reasoning"},
		{kind: blockAssistant, content: "assistant answer"},
	}
	model.refreshTranscript(true)
	plain := ansi.Strip(model.transcriptContent)
	for _, marker := range []string{"› ", "┊ ", "│ "} {
		if !strings.Contains(plain, marker) {
			t.Fatalf("transcript is missing marker %q: %q", marker, plain)
		}
	}
	if lipgloss.Width(model.transcriptContent) > model.viewport.Width {
		t.Fatalf("transcript width = %d, want <= %d", lipgloss.Width(model.transcriptContent), model.viewport.Width)
	}
}

func TestHighlightSelectionWrapsCharactersWithBackground(t *testing.T) {
	line := lipgloss.NewStyle().Foreground(lipgloss.Color("39")).Render("<?php") + " $name"
	style := lipgloss.NewStyle().Background(lipgloss.Color("238"))
	rendered := highlightSelection(line, 1, 5, style)
	if got := ansi.Strip(rendered); got != "<?php $name" {
		t.Fatalf("highlighted text = %q", got)
	}
	selected := style.Render("?php")
	if !strings.Contains(rendered, selected) {
		t.Fatalf("selected characters are not styled: %q", rendered)
	}
}

func TestTranscriptSelectionUsesDisplayColumnsForWideCharacters(t *testing.T) {
	model := newTestModel(t)
	model.blocks = []transcriptBlock{{kind: blockAssistant, content: "你好 world"}}
	model.refreshTranscript(true)
	plainLine := ansi.Strip(strings.Split(model.transcriptContent, "\n")[0])
	before, _, ok := strings.Cut(plainLine, "好")
	if !ok {
		t.Fatalf("rendered line does not contain wide character: %q", plainLine)
	}
	startColumn := ansi.StringWidth(before)
	model.selection = transcriptSelection{
		active:      true,
		start:       0,
		end:         0,
		startColumn: startColumn,
		endColumn:   startColumn + ansi.StringWidth("好 "),
	}
	selected, lines := model.selectedTranscript()
	if lines != 1 {
		t.Fatalf("selected lines = %d, want 1", lines)
	}
	if selected != "好 " {
		t.Fatalf("selected text = %q, want %q", selected, "好 ")
	}
	rendered := model.renderViewport()
	if !strings.Contains(ansi.Strip(rendered), "你好 world") {
		t.Fatalf("rendered selection lost text: %q", ansi.Strip(rendered))
	}
}

func TestModelConsumesReasoningToolAndAssistantEvents(t *testing.T) {
	model := newTestModel(t)
	model.busy = true
	model.stage = "Thinking"
	model.applyRuntimeEvent(agentruntime.Event{Type: agentruntime.EventAssistantReasoningDelta, Delta: "inspect runtime"})
	model.applyRuntimeEvent(agentruntime.Event{
		Type: agentruntime.EventAssistantMessage,
		Message: schema.AssistantMessage("", []schema.ToolCall{{
			ID: "call-1",
			Function: schema.FunctionCall{
				Name:      "runtime_diagnostics",
				Arguments: `{}`,
			},
		}}),
	})
	model.applyRuntimeEvent(agentruntime.Event{
		Type: agentruntime.EventToolResult,
		Message: &schema.Message{
			Role:       schema.Tool,
			ToolCallID: "call-1",
			ToolName:   "runtime_diagnostics",
			Content:    `{"go_version":"go1.27.0"}`,
		},
	})
	model.blocks[model.activeTools["call-1"]].resultExpanded = true
	model.applyRuntimeEvent(agentruntime.Event{Type: agentruntime.EventAssistantReasoningDelta, Delta: "prepare answer"})
	model.applyRuntimeEvent(agentruntime.Event{Type: agentruntime.EventAssistantTextDelta, Delta: "final answer"})

	final := schema.AssistantMessage("final answer", nil)
	final.ResponseMeta = &schema.ResponseMeta{Usage: &schema.TokenUsage{
		PromptTokens: 10,
		PromptTokenDetails: schema.PromptTokenDetails{
			CachedTokens: 4,
		},
		CompletionTokens: 5,
		TotalTokens:      15,
		CompletionTokensDetails: schema.CompletionTokensDetails{
			ReasoningTokens: 2,
		},
	}}
	model.finishTurn(turnDoneMsg{result: agentruntime.Result{Final: final}})
	plain := ansi.Strip(model.View())
	for _, expected := range []string{"inspect runtime", "runtime_diagnostics", "go1.27.0", "prepare answer", "final answer", "reasoning 2", "total 15", "cache 4 40.0%"} {
		if !strings.Contains(plain, expected) {
			t.Fatalf("view does not contain %q:\n%s", expected, plain)
		}
	}
	if model.busy || model.stage != "Ready" || model.completedTurns != 1 {
		t.Fatalf("turn state: busy=%v stage=%q turns=%d", model.busy, model.stage, model.completedTurns)
	}
}

func TestModelShowsProviderRequestStages(t *testing.T) {
	model := newTestModel(t)
	model.busy = true
	model.stage = "Preparing request"

	model.applyRuntimeEvent(agentruntime.Event{Type: agentruntime.EventModelPreparationStarted})
	if model.stage != "Preparing context" {
		t.Fatalf("preparation stage = %q", model.stage)
	}
	model.applyRuntimeEvent(agentruntime.Event{Type: agentruntime.EventExperiencePreparationStarted})
	if model.stage != "Preparing memory" {
		t.Fatalf("memory stage = %q", model.stage)
	}
	model.applyRuntimeEvent(agentruntime.Event{Type: agentruntime.EventContextCompactionStarted})
	if model.stage != "Compacting context" {
		t.Fatalf("compaction stage = %q", model.stage)
	}
	model.applyRuntimeEvent(agentruntime.Event{Type: agentruntime.EventModelBudgetReservationStarted})
	if model.stage != "Reserving model budget" {
		t.Fatalf("budget stage = %q", model.stage)
	}
	model.applyRuntimeEvent(agentruntime.Event{Type: agentruntime.EventModelRequestPersistenceStarted})
	if model.stage != "Persisting request" {
		t.Fatalf("persistence stage = %q", model.stage)
	}
	model.applyRuntimeEvent(agentruntime.Event{Type: agentruntime.EventModelRequestStarted})
	if model.stage != "Requesting model" {
		t.Fatalf("request stage = %q", model.stage)
	}
	model.applyRuntimeEvent(agentruntime.Event{
		Type:    agentruntime.EventModelRequestStarted,
		Purpose: "experience",
	})
	if model.stage != "Preparing memory · requesting model" {
		t.Fatalf("auxiliary request stage = %q", model.stage)
	}
	model.applyRuntimeEvent(agentruntime.Event{Type: agentruntime.EventModelResponseStarted})
	if model.stage != "Waiting for model output" {
		t.Fatalf("response stage = %q", model.stage)
	}
	model.applyRuntimeEvent(agentruntime.Event{Type: agentruntime.EventAssistantTextDelta, Delta: "answer"})
	if model.stage != "Responding" {
		t.Fatalf("text stage = %q", model.stage)
	}
	model.applyRuntimeEvent(agentruntime.Event{Type: agentruntime.EventTurnFinalizing})
	if model.stage != "Finalizing" {
		t.Fatalf("finalizing stage = %q", model.stage)
	}
	model.applyRuntimeEvent(agentruntime.Event{
		Type: agentruntime.EventAssistantMessage,
		Message: schema.AssistantMessage("", []schema.ToolCall{{
			ID:       "call-1",
			Function: schema.FunctionCall{Name: "workspace_read"},
		}}),
	})
	if model.stage != "Running workspace_read" {
		t.Fatalf("tool stage = %q", model.stage)
	}
	model.applyRuntimeEvent(agentruntime.Event{
		Type:    agentruntime.EventToolResult,
		Message: &schema.Message{Role: schema.Tool, ToolCallID: "call-1", ToolName: "workspace_read"},
	})
	if model.stage != "Processing tool result" {
		t.Fatalf("tool result stage = %q", model.stage)
	}
	for _, test := range []struct {
		event agentruntime.Event
		want  string
	}{
		{event: agentruntime.Event{Type: agentruntime.EventToolProposed, ToolName: "workspace_write"}, want: "Preparing tool call · workspace_write"},
		{event: agentruntime.Event{Type: agentruntime.EventToolPolicyChecking, ToolName: "workspace_write"}, want: "Checking tool policy · workspace_write"},
		{event: agentruntime.Event{Type: agentruntime.EventToolApprovalWaiting, ToolName: "workspace_write"}, want: "Waiting for approval · workspace_write"},
		{event: agentruntime.Event{Type: agentruntime.EventToolApprovalResolved, ToolName: "workspace_write", Approved: true}, want: "Approval granted · workspace_write"},
		{event: agentruntime.Event{Type: agentruntime.EventToolExecutionStarted, ToolName: "workspace_write"}, want: "Running workspace_write"},
		{event: agentruntime.Event{Type: agentruntime.EventToolExecutionSucceeded, ToolName: "workspace_write"}, want: "Tool completed · workspace_write"},
		{event: agentruntime.Event{Type: agentruntime.EventToolExecutionFailed, ToolName: "workspace_write"}, want: "Tool failed · workspace_write"},
		{event: agentruntime.Event{Type: agentruntime.EventToolOutcomeUnknown, ToolName: "workspace_write"}, want: "Tool outcome unknown; inspect state · workspace_write"},
	} {
		model.applyRuntimeEvent(test.event)
		if model.stage != test.want {
			t.Fatalf("event %q stage = %q, want %q", test.event.Type, model.stage, test.want)
		}
	}
}

func TestModelCancelsAnActiveTurn(t *testing.T) {
	model := newTestModel(t)
	turnContext, cancel := context.WithCancel(context.Background())
	model.busy = true
	model.turnContext = turnContext
	model.turnCancel = cancel

	_, handled := model.handleKey(tea.KeyMsg{Type: tea.KeyCtrlC})
	if !handled || !model.cancelling || model.stage != "Cancelling" {
		t.Fatalf("cancel state: handled=%v cancelling=%v stage=%q", handled, model.cancelling, model.stage)
	}
	select {
	case <-turnContext.Done():
	default:
		t.Fatal("turn context was not cancelled")
	}
}

func TestStatusRetainsContextAndCacheOnNarrowTerminal(t *testing.T) {
	model := newTestModel(t)
	model.contextUsage = agentruntime.ContextUsage{
		Used: 21000, Window: 32768, InputLimit: 22938, Status: tokenizer.StatusEstimated,
		State: "compaction_pending", NeedsCompaction: true,
	}
	model.stage = "Continuing task with a long stage label"
	model.lastUsage = tokenusage.Snapshot{Input: 12000, Cached: 9000, Output: 300, Total: 12300}
	status := ansi.Strip(model.renderStatus(40))
	if !strings.Contains(status, "ctx") || !strings.Contains(status, "cache") || !strings.Contains(status, "pending") {
		t.Fatalf("narrow status dropped context/cache usage: %q", status)
	}
	if ansi.StringWidth(status) > 40 {
		t.Fatalf("narrow status exceeds the available width: %q", status)
	}
	model.resize(40, 24)
	plain := ansi.Strip(model.View())
	if !strings.Contains(plain, "ctx") || !strings.Contains(plain, "cache") || !strings.Contains(plain, "pending") {
		t.Fatalf("40-column view dropped context/cache state: %q", plain)
	}
}

func TestFinishTurnDoesNotEraseStreamingUsage(t *testing.T) {
	model := newTestModel(t)
	model.lastUsage = tokenusage.Snapshot{Input: 100, Cached: 60, Output: 10, Total: 110}
	model.finishTurn(turnDoneMsg{result: agentruntime.Result{Final: schema.AssistantMessage("answer", nil)}})
	if model.lastUsage.Cached != 60 || model.lastUsage.Total != 110 {
		t.Fatalf("streaming usage was erased: %#v", model.lastUsage)
	}
}

func TestModelKeepsNewTurnsPinnedToBottom(t *testing.T) {
	model := newTestModel(t)
	for range 80 {
		model.blocks = append(model.blocks, transcriptBlock{kind: blockAssistant, content: "history line"})
	}
	model.refreshTranscript(true)
	if !model.viewport.AtBottom() || !model.autoScroll {
		t.Fatal("initial transcript is not pinned to bottom")
	}
	model.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelUp})
	if model.autoScroll {
		t.Fatal("manual upward scrolling did not pause auto-scroll")
	}
	model.submitInputForTest("new prompt")
	if !model.autoScroll || !model.viewport.AtBottom() {
		t.Fatal("new turn did not restore bottom pinning")
	}
}

func TestModelCompletesCommandsAndRecallsHistory(t *testing.T) {
	model := newTestModel(t)
	model.inputHistory = []string{"first prompt", "second prompt"}
	model.historyIndex = len(model.inputHistory)
	model.input.SetValue("/he")
	model.updateCommandSuggestions()
	if _, handled := model.handleKey(tea.KeyMsg{Type: tea.KeyTab}); !handled || model.input.Value() != "/help" {
		t.Fatalf("completed input = %q, handled=%v", model.input.Value(), handled)
	}
	if command := model.submit(); command == nil {
		t.Fatal("help command returned no command")
	}
	if len(model.blocks) == 0 || !strings.Contains(model.blocks[len(model.blocks)-1].content, "/session") {
		t.Fatalf("help block = %#v", model.blocks)
	}
	if !model.recallPrevious() || model.input.Value() != "second prompt" {
		t.Fatalf("previous history = %q", model.input.Value())
	}
	if !model.recallPrevious() || model.input.Value() != "first prompt" {
		t.Fatalf("first history = %q", model.input.Value())
	}
	if !model.recallNext() || model.input.Value() != "second prompt" {
		t.Fatalf("next history = %q", model.input.Value())
	}
}

func TestMarkdownRendererHighlightsFencedCode(t *testing.T) {
	renderer := &markdownRenderer{}
	output := renderer.render("```go\nfunc main() {}\n```", 72)
	if !strings.Contains(ansi.Strip(output), "func main()") {
		t.Fatalf("rendered markdown = %q", ansi.Strip(output))
	}
}

func TestMarkdownRendererPresentsCodeAsDedicatedPanel(t *testing.T) {
	renderer := &markdownRenderer{}
	output := renderer.render("前言\n```php\n<?php\necho \"hello\";\n```\n结尾", 72)
	plain := ansi.Strip(output)
	if strings.Contains(plain, "```") {
		t.Fatalf("markdown fences leaked into terminal output: %q", plain)
	}
	for _, expected := range []string{"前言", "<?php", "echo \"hello\";", "结尾"} {
		if !strings.Contains(plain, expected) {
			t.Fatalf("code panel is missing %q: %q", expected, plain)
		}
	}
	if !strings.Contains(output, codeBackground) {
		t.Fatalf("code panel does not emit a background: %q", output)
	}
}

func TestMarkdownRendererNormalizesInlineOpeningFence(t *testing.T) {
	renderer := &markdownRenderer{}
	output := renderer.render("给你一段代码。```go\npackage main\nfunc main() {}", 72)
	plain := ansi.Strip(output)
	if strings.Contains(plain, "```") {
		t.Fatalf("inline markdown fence leaked into terminal output: %q", plain)
	}
	if !strings.Contains(plain, "package main") || !strings.Contains(output, codeBackground) {
		t.Fatalf("inline fenced code was not rendered as a code panel: %q", output)
	}
}

func TestCodePanelFillsBlankSourceLines(t *testing.T) {
	const width = 32
	output := renderCodeBlock("first\n\nthird", "go", width)
	lines := strings.Split(output, "\n")
	if len(lines) != 3 {
		t.Fatalf("code panel lines = %d, want 3: %q", len(lines), output)
	}
	for index, line := range lines {
		if !strings.HasPrefix(line, codeBackground) {
			t.Fatalf("line %d does not start with code background: %q", index, line)
		}
		if got := ansi.StringWidth(line); got != width {
			t.Fatalf("line %d width = %d, want %d: %q", index, got, width, ansi.Strip(line))
		}
	}
}

func TestCodePanelExpandsTabsBeforeSizing(t *testing.T) {
	const width = 24
	output := renderCodeBlock("\treturn 1", "go", width)
	line, _, _ := strings.Cut(output, "\n")
	if ansi.StringWidth(line) != width {
		t.Fatalf("tabbed code line width = %d, want %d: %q", ansi.StringWidth(line), width, ansi.Strip(line))
	}
	if !strings.Contains(ansi.Strip(line), "    return 1") {
		t.Fatalf("tab was not expanded in code panel: %q", ansi.Strip(line))
	}
}

func TestCodePanelBackgroundSurvivesViewportLayout(t *testing.T) {
	model := newTestModel(t)
	model.resize(80, 24)
	model.blocks = []transcriptBlock{{
		kind:    blockAssistant,
		content: "```go\npackage main\n\nfunc main() {}\n```",
	}}
	model.refreshTranscript(true)

	lines := strings.Split(model.renderViewport(), "\n")
	for index, line := range lines {
		if !strings.Contains(ansi.Strip(line), "package main") {
			continue
		}
		if index+1 >= len(lines) {
			t.Fatal("code panel has no blank line after package declaration")
		}
		if !strings.Contains(lines[index+1], codeBackground) {
			t.Fatalf("blank code line lost background after viewport layout: %q", lines[index+1])
		}
		return
	}
	t.Fatal("package declaration was not rendered")
}

func newTestModel(t *testing.T) *Model {
	t.Helper()
	model, err := NewModel(context.Background(), Config{
		Conversation: &fakeConversation{id: "session-test"},
		Model:        "gpt-test",
		Provider:     "openai",
	})
	if err != nil {
		t.Fatal(err)
	}
	model.resize(100, 30)
	return model
}

func (model *Model) submitInputForTest(value string) {
	model.input.SetValue(value)
	model.submit()
}
