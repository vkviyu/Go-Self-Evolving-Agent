package app

import (
	"strings"
	"testing"
)

func TestRootWorkspaceInstructionExplainsBoundaryAndOverride(t *testing.T) {
	instruction := rootWorkspaceInstruction("/Users/example/project")

	for _, expected := range []string{
		`The effective workspace root is "/Users/example/project".`,
		"Workspace tool paths and shell working_directory are relative to this root",
		"use \".\" for the root itself",
		"stop and explain the boundary",
		"restart with --workspace pointing to the intended project",
		"Do not try parent paths or shell commands to bypass the workspace boundary",
	} {
		if !strings.Contains(instruction, expected) {
			t.Fatalf("workspace instruction missing %q:\n%s", expected, instruction)
		}
	}
}
