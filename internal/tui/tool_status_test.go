package tui

import (
	"encoding/json"
	"strings"
	"testing"

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
	if strings.Contains(rendered, "workspace_read") || strings.Contains(rendered, "workspace_grep") {
		t.Fatalf("tool names leaked into collapsed group: %q", rendered)
	}
	if !model.toggleToolDisclosureAtLine(0) || !model.blocks[0].toolGroupExpanded {
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
