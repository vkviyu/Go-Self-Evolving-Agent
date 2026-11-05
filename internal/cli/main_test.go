package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/config"
)

// TestMain points GSEA_HOME and the coordination directory at private
// temporary directories, so commands under test never read the developer's
// configuration or write coordination state into the real home. Tests may
// still select another home with t.Setenv.
func TestMain(m *testing.M) {
	os.Exit(runIsolated(m))
}

// runIsolated runs the package tests with isolated user-level directories and
// removes them afterwards.
func runIsolated(m *testing.M) int {
	root, err := os.MkdirTemp("", "gsea-cli-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(root)
	for key, value := range map[string]string{
		config.HomeEnvironment:         filepath.Join(root, "home"),
		config.CoordinationEnvironment: filepath.Join(root, "coordination"),
	} {
		if err := os.Setenv(key, value); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	return m.Run()
}
