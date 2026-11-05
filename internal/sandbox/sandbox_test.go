package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSanitizedEnvironmentDropsSecrets(t *testing.T) {
	environment := sanitizedEnvironment([]string{"PATH=/bin", "XAPI_API_KEY=hidden", "OPENAI_TOKEN=hidden", "DATABASE_URL=hidden", "PGSERVICE=hidden", "DYLD_INSERT_LIBRARIES=hidden", "GIT_SSH_COMMAND=hidden", "LANG=C"})
	joined := strings.Join(environment, "\n")
	if strings.Contains(joined, "hidden") || !strings.Contains(joined, "PATH=/bin") || !strings.Contains(joined, "GSEA_SANDBOX=1") {
		t.Fatalf("sanitized environment = %q", joined)
	}
}

func TestReadOnlyInvocationCannotWriteWorkspaceButHasScratch(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("no OS sandbox")
	}
	root := canonicalTempDir(t)
	runner, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr strings.Builder
	err = runner.Run(context.Background(), Request{WorkspaceRoot: root, WorkingDirectory: root, ReadOnly: true, Command: `printf scratch > "$TMPDIR/value"; cat "$TMPDIR/value"; printf forbidden > changed.txt`}, &stdout, &stderr)
	if err == nil || stdout.String() != "scratch" {
		t.Fatalf("stdout=%q stderr=%q err=%v", stdout.String(), stderr.String(), err)
	}
	if _, err := os.Stat(filepath.Join(root, "changed.txt")); !os.IsNotExist(err) {
		t.Fatalf("read-only workspace changed: %v", err)
	}
}

func TestContainerRunnerUsesWorkingDirectoryAndDropsSecrets(t *testing.T) {
	workspace := canonicalTempDir(t)
	stdout, stderr := &strings.Builder{}, &strings.Builder{}
	runner := &containerRunner{workspace: workspace}
	err := runner.Run(context.Background(), Request{
		Command:          `printf '%s|%s' "$PWD" "${XAPI_API_KEY-unset}"`,
		WorkingDirectory: workspace,
		Environment:      []string{"PATH=/bin:/usr/bin", "XAPI_API_KEY=hidden"},
	}, stdout, stderr)
	if err != nil {
		t.Fatalf("run container command: %v: %s", err, stderr)
	}
	if got := stdout.String(); got != workspace+"|unset" {
		t.Fatalf("stdout = %q", got)
	}
}

func TestBubblewrapUsesSupportedNetworkNamespaceFlag(t *testing.T) {
	workspace := canonicalTempDir(t)
	arguments := bubblewrapArguments(workspace, Request{
		Command:          "true",
		WorkingDirectory: workspace,
	})
	joined := strings.Join(arguments, " ")
	if !strings.Contains(joined, "--unshare-net") {
		t.Fatalf("arguments omit supported network namespace flag: %s", joined)
	}
	if strings.Contains(joined, "--unshare-network") {
		t.Fatalf("arguments contain unsupported bubblewrap flag: %s", joined)
	}
}

func TestSeatbeltProfileScopesWrites(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Seatbelt is only available on macOS")
	}
	workspace := canonicalTempDir(t)
	outside := canonicalTempDir(t)
	runner, err := New(workspace)
	if err != nil {
		t.Fatal(err)
	}
	stdout, stderr := &strings.Builder{}, &strings.Builder{}
	err = runner.Run(context.Background(), Request{
		Command:          "printf inside > inside.txt; printf outside > " + shellQuote(filepath.Join(outside, "outside.txt")),
		WorkingDirectory: workspace,
	}, stdout, stderr)
	if err == nil {
		t.Fatal("sandbox unexpectedly allowed an outside write")
	}
	if data, readErr := os.ReadFile(filepath.Join(workspace, "inside.txt")); readErr != nil || string(data) != "inside" {
		t.Fatalf("workspace write failed: data=%q err=%v runErr=%v stderr=%q", data, readErr, err, stderr.String())
	}
	if _, statErr := os.Stat(filepath.Join(outside, "outside.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("outside file exists: %v", statErr)
	}
}

func canonicalTempDir(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	canonical, err := filepath.EvalSymlinks(directory)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
