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
	binary, err := buildCLIATE(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	cliATE.binary = binary
	for key, value := range map[string]string{
		config.HomeEnvironment:         filepath.Join(root, "home"),
		config.CoordinationEnvironment: filepath.Join(root, "coordination"),
		config.ProfileEnvironment:      "",
		"GSEA_ATE_ENABLED":             "true",
		"GSEA_ATE_BINARY":              binary,
		"GSEA_ATE_DIRECTORY":           "",
	} {
		if err := os.Setenv(key, value); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	return m.Run()
}
