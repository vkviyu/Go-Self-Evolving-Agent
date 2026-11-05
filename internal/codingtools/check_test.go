package codingtools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunCheckReportsExitTimeoutAndCancellation(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "ok"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	workspace, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	workspace.shellRunner = directTestRunner{}
	workspace.shellSandboxErr = nil
	ctx := context.Background()
	// The check runs in the main workspace root.
	if err := workspace.RunCheck(ctx, "test -f ok", time.Second); err != nil {
		t.Fatalf("passing check: %v", err)
	}
	if err := workspace.RunCheck(ctx, "echo broken; exit 3", time.Second); err == nil || !strings.Contains(err.Error(), "exited 3: broken") {
		t.Fatalf("failing check error = %v", err)
	}
	if err := workspace.RunCheck(ctx, "sleep 1", 20*time.Millisecond); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("slow check error = %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := workspace.RunCheck(canceled, "true", time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled check error = %v", err)
	}
	if err := workspace.RunCheck(ctx, " ", time.Second); err == nil {
		t.Fatal("empty check accepted")
	}
}
