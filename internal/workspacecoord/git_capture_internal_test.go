package workspacecoord

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommandGitUsesMinimalEnvironmentAndDisablesExecutableHooks(t *testing.T) {
	bin := t.TempDir()
	executable := filepath.Join(bin, "git")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nprintf '%s\\n' \"${DATABASE_URL-unset}\" \"${GIT_CONFIG_NOSYSTEM-unset}\" \"${GIT_CONFIG_GLOBAL-unset}\"\nprintf '%s\\n' \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DATABASE_URL", "fake-secret-never-forward")
	workspace := t.TempDir()
	result, err := (commandGit{workspace: workspace}).Run(context.Background(), workspace, "status")
	if err != nil {
		t.Fatal(err)
	}
	text := string(result)
	if strings.Contains(text, "fake-secret") || !strings.HasPrefix(text, "unset\n1\n"+os.DevNull+"\n") {
		t.Fatalf("Git inherited host configuration or credentials: %q", text)
	}
	for _, argument := range []string{"core.hooksPath=" + os.DevNull, "core.fsmonitor=false", "core.splitIndex=false", "--no-pager"} {
		if !strings.Contains(text, argument) {
			t.Fatalf("missing trusted Git option %q: %q", argument, text)
		}
	}
}

func TestCommandGitRejectsWorkspaceExecutableShadowing(t *testing.T) {
	workspace := t.TempDir()
	bin := filepath.Join(workspace, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if _, err := (commandGit{workspace: workspace}).Run(context.Background(), workspace, "status"); err == nil || !strings.Contains(err.Error(), "Agent-writable") {
		t.Fatalf("workspace Git executable was trusted: %v", err)
	}
}
