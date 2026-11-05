package pluginbuild

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestToolchainEvidenceCoversDriverCompilerAndStdlib(t *testing.T) {
	root := t.TempDir()
	paths := []string{"bin/go", "pkg/tool/platform/compile", "pkg/include/textflag.h", "src/runtime/runtime.go", "VERSION"}
	for _, path := range paths {
		file := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte("original"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	initial, err := fingerprintToolchain(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		file := filepath.Join(root, filepath.FromSlash(path))
		if err := os.WriteFile(file, []byte("mutated!"), 0600); err != nil {
			t.Fatal(err)
		}
		changed, err := fingerprintToolchain(context.Background(), root)
		if err != nil || changed == initial {
			t.Fatalf("toolchain evidence ignored %s: %v", path, err)
		}
		if err := os.WriteFile(file, []byte("original"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := fingerprintToolchain(canceled, root); !errors.Is(err, context.Canceled) {
		t.Fatalf("toolchain hashing ignored cancellation: %v", err)
	}
}
