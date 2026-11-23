package codingtools

import (
	"encoding/json"

	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

const (
	WriteToolName = "workspace_write"
	EditToolName  = "workspace_edit"
	ShellToolName = "shell_exec"
)

// Bindings returns the complete first-party coding tool catalog. Metadata is
// kept here so adding a tool does not require editing filesystem primitives.
func (workspace *Workspace) Bindings() []tool.Binding {
	bindings := []tool.Binding{
		workspace.readBinding(),
		workspace.listBinding(),
		workspace.globBinding(),
		workspace.grepBinding(),
	}
	if workspace.diagnostics != nil {
		bindings = append(bindings, workspace.diagnosticsBinding())
	}
	return append(bindings,
		workspace.writeBinding(),
		workspace.editBinding(),
		workspace.shellBinding(),
	)
}

func (workspace *Workspace) readBinding() tool.Binding {
	return bindingRevision("workspace_read", "builtin-v4",
		"Read a UTF-8 text file under the effective workspace root. Prefer this tool for reading local documents and source files. For a large document, read a section or a full page per call. Follow next_offset for sequential reading or next_start_line to continue a selected line range. Keep the default byte budget unless a specific range needs a different size. Use sha256 as expected_sha256 only when fingerprint_complete is true. Files over 4 MiB can still be paged but have no full-file fingerprint.",
		`{"type":"object","additionalProperties":false,"required":["path"],"properties":{"path":{"type":"string","description":"Workspace-relative file path, for example docs/design.md"},"start_line":{"type":"integer","minimum":1,"description":"First line, default 1"},"end_line":{"type":"integer","minimum":1,"description":"Last line, inclusive; omit to continue to the byte limit or end of file"},"offset":{"type":"integer","minimum":0,"description":"Byte position returned as next_offset; omit both line fields when using offset"},"max_bytes":{"type":"integer","minimum":1,"maximum":4194304,"description":"Maximum bytes in the selected page, default 16384. Larger results may be represented as artifact references"}}}`,
		tool.RiskRead, tool.InvokeFunc(workspace.read))
}

func (workspace *Workspace) listBinding() tool.Binding {
	return binding("workspace_list", "List entries under the effective workspace root in stable name order. Use . for the workspace root and workspace-relative paths for subdirectories; never pass the host absolute path.", `{"type":"object","additionalProperties":false,"properties":{"path":{"type":"string","description":"Workspace-relative directory path; use . for the workspace root"},"max_entries":{"type":"integer","minimum":1,"maximum":2000}}}`, tool.RiskRead, tool.InvokeFunc(workspace.list))
}

func (workspace *Workspace) globBinding() tool.Binding {
	return binding("workspace_glob", "Find files below the effective workspace root using a filepath glob such as **/*.go or internal/**/*.go. The pattern is relative to the workspace and is not a regular expression.", `{"type":"object","additionalProperties":false,"required":["pattern"],"properties":{"pattern":{"type":"string","description":"Relative filepath glob, for example internal/**/*.go"},"max_results":{"type":"integer","minimum":1,"maximum":1000}}}`, tool.RiskRead, tool.InvokeFunc(workspace.glob))
}

func (workspace *Workspace) grepBinding() tool.Binding {
	return binding("workspace_grep", "Search text files below the effective workspace root with one valid Go/Rust/RE2 regular expression. Escape literal parentheses or use separate simpler searches; do not concatenate incomplete function signatures. Use path and glob only to narrow the search, and keep paths workspace-relative.", `{"type":"object","additionalProperties":false,"required":["pattern"],"properties":{"pattern":{"type":"string","description":"One valid RE2 regular expression; for literal text, quote punctuation such as parentheses"},"path":{"type":"string","description":"Optional workspace-relative directory or file"},"glob":{"type":"string","description":"Optional relative filepath glob such as **/*.go"},"ignore_case":{"type":"boolean"},"max_results":{"type":"integer","minimum":1,"maximum":1000},"max_bytes":{"type":"integer","minimum":1,"maximum":4194304}}}`, tool.RiskRead, tool.InvokeFunc(workspace.grep))
}

func (workspace *Workspace) diagnosticsBinding() tool.Binding {
	return bindingRevision("workspace_diagnostics", "builtin-v2", "Ask the configured language server to analyze a workspace file and return ranked, deduplicated semantic diagnostics.", `{"type":"object","additionalProperties":false,"required":["path"],"properties":{"path":{"type":"string","description":"Workspace-relative Go source, go.mod, or go.work path"}}}`, tool.RiskRead, tool.InvokeFunc(workspace.languageServerDiagnostics))
}

func (workspace *Workspace) writeBinding() tool.Binding {
	return bindingRevision(WriteToolName, "builtin-v5", "Create or atomically replace a regular file in the workspace and return a bounded unified diff. New and existing content must each be at most 4 MiB; symbolic-link targets are rejected. Paths are relative to the workspace root; never pass an absolute path.", `{"type":"object","additionalProperties":false,"required":["path","content"],"properties":{"path":{"type":"string","minLength":1,"description":"Workspace-relative file path, for example hello.txt; absolute paths are rejected"},"content":{"type":"string"},"mode":{"type":"integer","minimum":0,"maximum":511},"expected_sha256":{"type":"string","pattern":"^[A-Fa-f0-9]{64}$"}}}`, tool.RiskWrite, tool.InvokeFunc(workspace.write))
}

func (workspace *Workspace) editBinding() tool.Binding {
	return bindingRevision(EditToolName, "builtin-v5", "Replace exact text in a workspace file atomically and return a bounded unified diff. Both original and resulting content must be at most 4 MiB. Paths are relative to the workspace root; never pass an absolute path.", `{"type":"object","additionalProperties":false,"required":["path","old_text","new_text"],"properties":{"path":{"type":"string","minLength":1,"description":"Workspace-relative file path; absolute paths are rejected"},"old_text":{"type":"string","minLength":1},"new_text":{"type":"string"},"replace_all":{"type":"boolean"},"expected_sha256":{"type":"string","pattern":"^[A-Fa-f0-9]{64}$"}}}`, tool.RiskWrite, tool.InvokeFunc(workspace.edit))
}

func (workspace *Workspace) shellBinding() tool.Binding {
	return bindingRevision(ShellToolName, "builtin-v5", "Run a shell command in the workspace or a workspace subdirectory and report bounded changes plus unified diffs. Each invocation may require approval and capture workspace changes. For local text, use workspace_read to read and workspace_grep to search.", `{"type":"object","additionalProperties":false,"required":["command"],"properties":{"command":{"type":"string","minLength":1},"working_directory":{"type":"string","description":"Workspace-relative directory path; omit for the workspace root"},"timeout_ms":{"type":"integer","minimum":1,"maximum":300000,"description":"Optional timeout for this command in milliseconds. Omit to run until the command exits or the caller cancels"},"max_output_bytes":{"type":"integer","minimum":1,"maximum":2097152}}}`, tool.RiskExecute, tool.InvokeFunc(workspace.shell))
}

func binding(name, description, schema string, risk tool.RiskClass, executor tool.Executor) tool.Binding {
	return bindingRevision(name, "builtin-v1", description, schema, risk, executor)
}

func bindingRevision(name, revision, description, schema string, risk tool.RiskClass, executor tool.Executor) tool.Binding {
	return tool.Binding{Descriptor: tool.Descriptor{Name: name, Description: description, InputSchema: json.RawMessage(schema), Revision: revision, RiskClass: risk, Capabilities: []string{"tool.invoke"}}, Executor: executor}
}
