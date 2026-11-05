package tui

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// toolFailureSummary recognizes the adapter's error envelope and the shell's
// execution status. Never search arbitrary file contents or stdout for "error".
// Full diagnostics remain available in the expandable result pane.
func toolFailureSummary(name, result string) string {
	var payload map[string]json.RawMessage
	if json.Unmarshal([]byte(result), &payload) != nil {
		return ""
	}
	var failure string
	if json.Unmarshal(payload["error"], &failure) == nil {
		failure = strings.Join(strings.Fields(ansi.Strip(failure)), " ")
		if failure != "" {
			return ansi.Truncate(failure, 240, "…")
		}
	}
	if name == "shell_exec" {
		var timedOut bool
		if json.Unmarshal(payload["timed_out"], &timedOut) == nil && timedOut {
			return "command timed out"
		}
		var exitCode int
		if json.Unmarshal(payload["exit_code"], &exitCode) == nil && exitCode != 0 {
			return fmt.Sprintf("command exited with code %d", exitCode)
		}
	}
	return ""
}
