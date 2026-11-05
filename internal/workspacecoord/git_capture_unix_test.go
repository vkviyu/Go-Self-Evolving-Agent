//go:build darwin || linux

package workspacecoord

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestProtectedGitMetadataRejectsFIFOBeforeReading(t *testing.T) {
	source := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(source, "exclude"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := copyGitDataFile(source, t.TempDir(), "exclude"); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("FIFO metadata accepted: %v", err)
	}
}
