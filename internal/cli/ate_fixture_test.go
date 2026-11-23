package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	sessionate "github.com/vkviyu/go-self-evolving-agent/internal/session/atestore"
	"github.com/vkviyu/go-self-evolving-agent/internal/testfixture"
)

// cliATE is the package-wide native engine binary. TestMain builds it once so
// CLI subprocesses and direct app.Open calls exercise the real process manager.
var cliATE struct{ binary string }

func buildCLIATE(root string) (string, error) {
	binary := filepath.Join(root, "ate-test")
	command := exec.Command("go", "build", "-o", binary, "github.com/vkviyu/agent-temporal-engine/cmd/ate")
	if output, err := command.CombinedOutput(); err != nil {
		return "", &fixtureBuildError{output: output, err: err}
	}
	return binary, nil
}

type fixtureBuildError struct {
	output []byte
	err    error
}

func (e *fixtureBuildError) Error() string {
	return "build ate test binary: " + e.err.Error() + "\n" + string(e.output)
}

func (e *fixtureBuildError) Unwrap() error { return e.err }

// cliATEEnvironment points environment-based config loading at the fixture
// engine. The authority directory still defaults from each test's data dir.
func cliATEEnvironment(t *testing.T) {
	t.Helper()
	if cliATE.binary == "" {
		t.Fatal("ATE test binary is unavailable; TestMain must build it")
	}
	t.Setenv("GSEA_ATE_ENABLED", "true")
	t.Setenv("GSEA_ATE_BINARY", cliATE.binary)
	t.Setenv("GSEA_ATE_DIRECTORY", "")
	t.Setenv(config.ProfileEnvironment, "")
}

// cliATEConfig fills a programmatic configuration with isolated directories and
// the explicit fixture engine, matching what environment loading would produce.
func cliATEConfig(t *testing.T, cfg config.Config) config.Config {
	t.Helper()
	if cliATE.binary == "" {
		t.Fatal("ATE test binary is unavailable; TestMain must build it")
	}
	if cfg.HomeDirectory == "" {
		cfg.HomeDirectory = t.TempDir()
	}
	if cfg.DataDirectory == "" {
		cfg.DataDirectory = cfg.HomeDirectory
	}
	if cfg.WorkspaceDirectory == "" {
		cfg.WorkspaceDirectory = t.TempDir()
	}
	cfg.ATE = config.ATEConfig{
		Enabled: true, Binary: cliATE.binary,
		Directory: filepath.Join(cfg.DataDirectory, "temporal"), Timeout: 30 * time.Second,
	}
	return cfg
}

// cliCommandConfig is a command-line-shaped configuration: the data directory is
// also resolved as the home, so subprocess ATE directories match the fixture.
func cliCommandConfig(t *testing.T, data, workspace string) config.Config {
	t.Helper()
	t.Setenv(config.HomeEnvironment, data)
	cliATEEnvironment(t)
	return cliATEConfig(t, config.Config{
		HomeDirectory: data, DataDirectory: data, WorkspaceDirectory: workspace,
	})
}

// cliAppFixture exposes the native authorities opened at a resolved app path.
type cliAppFixture struct{ *testfixture.Database }

func (f *cliAppFixture) Events() *sessionate.Store { return f.SessionEvents() }

// nativeCLIProfile writes a user profile that enables the fixture engine in a
// dedicated authority directory, proving the selected profile — not the default
// home — decides which native store a command reads.
func nativeCLIProfile(t *testing.T) string {
	t.Helper()
	if cliATE.binary == "" {
		t.Fatal("ATE test binary is unavailable; TestMain must build it")
	}
	home := t.TempDir()
	t.Setenv(config.HomeEnvironment, home)
	name, directory := "native", filepath.Join(t.TempDir(), "authority")
	body := fmt.Sprintf("[ate]\nenabled = true\nbinary = %q\ndirectory = %q\ntimeout = \"30s\"\n", cliATE.binary, directory)
	if err := os.WriteFile(filepath.Join(home, name+".config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return name
}

// openCLIApp opens the real native session/orchestration stores at the same
// runtime and authority paths production assembly uses for cfg.
func openCLIApp(t *testing.T, cfg config.Config) *cliAppFixture {
	t.Helper()
	resolved, err := cfg.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	database, err := testfixture.OpenWithNativeDir(t, context.Background(), resolved.RuntimeDatabasePath(), resolved.ATE.Directory)
	if err != nil {
		t.Fatal(err)
	}
	return &cliAppFixture{Database: database}
}
