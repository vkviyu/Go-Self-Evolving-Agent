package tui

import (
	"encoding/json"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/cloudwego/eino/schema"

	"github.com/vkviyu/go-self-evolving-agent/internal/approval"
	"github.com/vkviyu/go-self-evolving-agent/internal/policy"
)

func TestFailedApprovedToolRemainsVisibleWhenCollapsed(t *testing.T) {
	model := newTestModel(t)
	model.applyAssistantMessage(schema.AssistantMessage("", []schema.ToolCall{{
		ID: "call", Function: schema.FunctionCall{Name: "shell_exec", Arguments: `{"command":"pwd"}`},
	}}))
	model.applyApprovalRequest(approval.Request{ID: "call", Policy: policy.Request{CallID: "call", ToolName: "shell_exec"}})
	model.resolveApproval(true)
	// Render before the result to exercise cache invalidation, too.
	if rendered := ansi.Strip(model.renderBlock(&model.blocks[0])); !strings.Contains(rendered, "✓") {
		t.Fatalf("approval missing: %q", rendered)
	}
	model.applyToolResult(schema.ToolMessage(`{"error":"invalid tool arguments: cannot unmarshal string"}`, "call", schema.WithToolName("shell_exec")))
	for _, expanded := range []bool{false, true, false} {
		model.blocks[0].resultExpanded = expanded
		rendered := ansi.Strip(model.renderBlock(&model.blocks[0]))
		if !strings.HasPrefix(rendered, "!") || strings.Contains(rendered, "✓") || !strings.Contains(rendered, "failed: invalid tool arguments") {
			t.Fatalf("expanded=%v failure hidden or shown as success: %q", expanded, rendered)
		}
	}
}

func TestCompletedToolBlocksUseCompactSpacing(t *testing.T) {
	blocks := []*transcriptBlock{
		{kind: blockTool},
		{kind: blockTool},
		{kind: blockAssistant},
		{kind: blockTool},
	}
	got := joinTranscriptParts([]string{"read", "list", "answer", "write"}, blocks)
	want := "read\nlist\n\nanswer\n\nwrite"
	if got != want {
		t.Fatalf("compact transcript spacing = %q, want %q", got, want)
	}
}

func TestCompletedToolCallsCollapseIntoOneSummary(t *testing.T) {
	model := newTestModel(t)
	model.resize(120, 30)
	model.blocks = []transcriptBlock{
		{kind: blockTool, content: "workspace_read", toolName: "workspace_read", toolResult: `{"content":"ok"}`},
		{kind: blockTool, content: "workspace_grep", toolName: "workspace_grep", toolResult: `{"error":"invalid grep pattern"}`},
	}
	model.refreshTranscript(true)
	rendered := ansi.Strip(model.transcriptContent)
	if !strings.Contains(rendered, "2 tool calls") || !strings.Contains(rendered, "1 failed") {
		t.Fatalf("tool group summary missing: %q", rendered)
	}
	if strings.Count(rendered, "\n") != 0 || !strings.Contains(rendered, "workspace_read") || !strings.Contains(rendered, "workspace_grep") {
		t.Fatalf("expected one descriptive line: %q", rendered)
	}
	if !model.toggleToolDisclosureAtLine(0) || model.toolInspector == nil {
		t.Fatalf("collapsed tool group did not expand")
	}
	model.refreshTranscript(true)
	rendered = ansi.Strip(model.transcriptContent)
	if !strings.Contains(rendered, "workspace_read") || !strings.Contains(rendered, "workspace_grep") {
		t.Fatalf("expanded tool group omitted members: %q", rendered)
	}
}

func TestHistoricalToolFailureVisibleWhenCollapsed(t *testing.T) {
	blocks := blocksFromHistory([]*schema.Message{
		schema.AssistantMessage("", []schema.ToolCall{{ID: "read", Function: schema.FunctionCall{Name: "workspace_read", Arguments: `{"path":"missing"}`}}}),
		schema.ToolMessage(`{"error":"no such file or directory"}`, "read", schema.WithToolName("workspace_read")),
	})
	model := newTestModel(t)
	if len(blocks) != 1 || blocks[0].resultExpanded {
		t.Fatalf("blocks=%+v", blocks)
	}
	rendered := ansi.Strip(model.renderBlock(&blocks[0]))
	if !strings.HasPrefix(rendered, "!") || !strings.Contains(rendered, "failed: no such file or directory") {
		t.Fatalf("historical failure hidden: %q", rendered)
	}
}

func TestToolFailureSummary(t *testing.T) {
	for _, test := range []struct{ name, result, want string }{
		{"shell_exec", `{"exit_code":2,"stdout":"","stderr":"bad command"}`, "command exited with code 2"},
		{"shell_exec", `{"exit_code":-1,"timed_out":true}`, "command timed out"},
		{"shell_exec", `{"exit_code":0,"stderr":"error is part of ordinary output"}`, ""},
		{"workspace_read", `{"content":"error: text","exit_code":3,"timed_out":true}`, ""},
		{"workspace_read", `{"content":"{\"error\":\"just file data\"}"}`, ""},
		{"workspace_read", `{"error":null}`, ""},
		{"workspace_read", `{"error":false}`, ""},
		{"workspace_read", `{"error":""}`, ""},
		{"workspace_read", `{"error":"  path\n escapes workspace  "}`, "path escapes workspace"},
		{"workspace_read", "plain text with error", ""},
	} {
		if got := toolFailureSummary(test.name, test.result); got != test.want {
			t.Errorf("%s %s: summary=%q want=%q", test.name, test.result, got, test.want)
		}
	}
	encoded, err := json.Marshal(map[string]string{"error": "\x1b[31m" + strings.Repeat("错误", 300)})
	if err != nil {
		t.Fatal(err)
	}
	summary := toolFailureSummary("workspace_read", string(encoded))
	if ansi.StringWidth(summary) > 240 || strings.Contains(summary, "\x1b") || !strings.HasSuffix(summary, "…") {
		t.Fatalf("unbounded or unsafe summary: %q", summary)
	}
}

func TestToolInspectionKeepsLongResultsOutOfChatAndRestoresReading(t *testing.T) {
	model := newTestModel(t)
	model.resize(58, 16)
	model.input.SetValue("unfinished draft")
	model.blocks = []transcriptBlock{
		{kind: blockUser, content: "Count files"},
		{kind: blockTool, toolName: "workspace_glob", toolArguments: `{"pattern":"*.md"}`, toolResult: "A.md"},
		{kind: blockTool, toolName: "workspace_glob", toolArguments: `{"pattern":"*.go"}`, toolResult: strings.Repeat("long result row\n", 80) + "result tail"},
		{kind: blockAssistant, content: strings.Repeat("answer\n", 50)},
	}
	model.refreshTranscript(false)
	model.autoScroll = false
	groupLine := model.transcriptLayout[1].start
	model.viewport.SetYOffset(groupLine)
	click := func(row int) {
		model.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: 3, Y: row})
	}
	click(model.transcriptTopRow())
	if got := ansi.Strip(model.transcriptContent); !strings.Contains(got, "*.go") || strings.Contains(got, "long result row") || strings.Contains(got, "answer") {
		t.Fatalf("unexpected tool list: %q", got)
	}
	click(model.transcriptTopRow() + 1)
	model.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	if model.viewport.YOffset == 0 || !strings.Contains(model.transcriptContent, "result tail") {
		t.Fatal("long result did not open in its own scrollable view")
	}
	offset := model.viewport.YOffset
	model.blocks = append(model.blocks, transcriptBlock{kind: blockSystem, content: "later progress"})
	model.refreshRootTranscript(true)
	if model.viewport.YOffset != offset || strings.Contains(model.transcriptContent, "later progress") {
		t.Fatal("live activity displaced tool inspection")
	}
	click(model.headerHeight()) // The return control stays visible even deep inside a result.
	model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	got := ansi.Strip(model.transcriptContent)
	if model.toolInspector != nil || model.viewport.YOffset != groupLine || model.input.Value() != "unfinished draft" ||
		!strings.Contains(got, "workspace_glob ×2") || !strings.Contains(got, "later progress") || strings.Contains(got, "result tail") {
		t.Fatal("chat, reading position, or draft was lost when closing details")
	}
}

func TestPendingApprovalTakesPrecedenceOverToolInspection(t *testing.T) {
	model := newTestModel(t)
	model.blocks = []transcriptBlock{{kind: blockTool, toolName: "read", toolResult: "old result"}}
	model.refreshTranscript(true)
	model.toggleToolDetails()
	model.Update(approvalRequestMsg{request: approval.Request{ID: "approve", Policy: policy.Request{ToolName: "write"}}})
	if model.toolInspector != nil || model.approvalPending == nil || !strings.Contains(model.View(), "Allow") {
		t.Fatal("a live approval was hidden by tool inspection")
	}
}
