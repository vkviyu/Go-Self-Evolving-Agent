//go:build darwin || linux

package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestProcessGroupCancelsDescendantsBeforeReturning(t *testing.T) {
	directory := t.TempDir()
	marker := filepath.Join(directory, "late-write")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	command := exec.CommandContext(ctx, "/bin/sh", "-c", "(sleep 0.4; printf leaked > "+shellQuote(marker)+") & wait")
	if err := runProcessGroup(command); err == nil {
		t.Fatal("canceled process succeeded")
	}
	time.Sleep(500 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("descendant wrote after cancellation", err)
	}
}
