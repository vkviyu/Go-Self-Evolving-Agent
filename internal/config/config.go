// Package config resolves process configuration before application components
// are constructed. It contains no global mutable state.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/tokenusage"
)

const (
	HomeEnvironment    = "GSEA_HOME"
	ProfileEnvironment = "GSEA_PROFILE"
	// CoordinationEnvironment overrides the user-level rendezvous directory
	// shared by every GSEA_HOME of this user.
	CoordinationEnvironment = "GSEA_COORDINATION_DIR"

	// AgentsWorkspaceAuto isolates writers in snapshots where supported.
	AgentsWorkspaceAuto = "auto"
	// AgentsWorkspaceSnapshot always isolates writers in snapshots.
	AgentsWorkspaceSnapshot = "snapshot"
	// AgentsWorkspaceReadonly narrows writers to read-only access.
	AgentsWorkspaceReadonly = "readonly"
	// AgentsWorkspaceShared lets writers share the leased main workspace.
	AgentsWorkspaceShared = "shared"
	// DefaultSnapshotCopyLimit is 1 GiB.
	DefaultSnapshotCopyLimit int64 = 1 << 30
)

var ErrProjectConfigUntrusted = errors.New("project configuration is not trusted")

type (
	// Config contains process-level paths and resource limits.
	Config struct {
		HomeDirectory string
		DataDirectory string
		// WorkspaceDirectory is the only directory that built-in coding tools may access.
		WorkspaceDirectory string
		// CoordinationDirectory holds cross-home workspace rendezvous locks. Load
		// resolves it outside GSEA_HOME; empty disables cross-home exclusion.
		CoordinationDirectory string
		Model                 ModelConfig
		// Agent configures the root Agent; Agents bounds delegated child Agents.
		Agent  AgentConfig
		Agents AgentsConfig
		Usage  UsageConfig
		ATE    ATEConfig
	}

	// ATEConfig controls the native ATE subprocess used for bounded storage batches.
	// Its zero value deliberately leaves ATE disabled for programmatic hosts.
	ATEConfig struct {
		Enabled           bool
		Binary, Directory string
		Timeout           time.Duration
		StartupTimeout    time.Duration // Zero selects the native manager's cold-start allowance.
	}

	// UsageConfig prices model usage and bounds the usage of each session
	// Team. Prices are keyed by "provider/model" or "model"; unpriced models
	// record zero cost. Zero budget limits are unlimited.
	UsageConfig struct {
		Prices map[string]tokenusage.Price
		Budget tokenusage.BudgetLimits
	}

	// ModelConfig contains non-secret model selection. APIKeyEnvironment names the
	// environment variable that ModelGateway resolves at construction time.
	ModelConfig struct {
		Provider               string
		WireAPI                string
		Name                   string
		TokenizerEncoding      string
		BaseURL                string
		APIKeyEnvironment      string
		ReasoningEffort        string
		Timeout                time.Duration
		ResponseHeaderTimeout  time.Duration
		StreamIdleTimeout      time.Duration
		StreamEventIdleTimeout time.Duration
	}

	// AgentConfig contains runtime defaults that are safe to keep in config.toml.
	// Secrets are intentionally absent; credentials are resolved by modelgateway.
	AgentConfig struct {
		Instruction             string
		MaxIterations           int
		MaxOutputTokens         int // 0 selects output reserve after model capacity discovery.
		ContextWindowTokens     int
		CompactionTriggerTokens int
	}

	// AgentsConfig bounds the delegated child Agents of one Supervisor. The
	// root Agent is not counted. ModelLimits caps concurrent runs per model
	// name; models without an entry share MaxRunning.
	AgentsConfig struct {
		MaxRunning  int
		MaxLive     int
		MaxDepth    int
		MaxChildren int
		ModelLimits map[string]int
		// DefaultWorkspace is the workspace of writable children: auto,
		// snapshot, readonly or shared.
		DefaultWorkspace string
		// AutoMerge applies conflict-free snapshot merges without approval.
		AutoMerge bool
		// AutoContinue wakes an idle root Agent when child results arrive.
		AutoContinue bool
		// SnapshotCopyLimit bounds the bytes plain-copied per snapshot when
		// copy-on-write cloning is unavailable.
		SnapshotCopyLimit int64
		// PostMergeCheck is a shell command run in the sandbox on the main
		// workspace after each applied merge. A nonzero exit fails the tasks
		// the merge delivered. Empty skips the check.
		PostMergeCheck string
		// PostMergeCheckTimeout bounds one run of PostMergeCheck.
		PostMergeCheckTimeout time.Duration
	}
)

// Default loads the effective configuration using the current working
// directory, user config, optional profile, environment and built-in defaults.
func Default() (Config, error) {
	return Load(LoadOptions{})
}

// Resolve validates paths and creates the private application directory.
func (c Config) Resolve() (Config, error) {
	var ateErr error
	c.ATE, ateErr = c.ResolveATE()
	if ateErr != nil {
		return Config{}, ateErr
	}
	if c.DataDirectory == "" {
		c.DataDirectory = c.HomeDirectory
	}
	if c.DataDirectory == "" {
		return Config{}, errors.New("data directory is required")
	}
	abs, err := filepath.Abs(c.DataDirectory)
	if err != nil {
		return Config{}, err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return Config{}, fmt.Errorf("create data directory: %w", err)
	}
	if err := os.Chmod(abs, 0o700); err != nil {
		return Config{}, fmt.Errorf("secure data directory: %w", err)
	}
	c.DataDirectory = abs
	if strings.TrimSpace(c.WorkspaceDirectory) == "" {
		c.WorkspaceDirectory, err = os.Getwd()
		if err != nil {
			return Config{}, fmt.Errorf("resolve workspace directory: %w", err)
		}
	}
	workspace, err := filepath.Abs(c.WorkspaceDirectory)
	if err != nil {
		return Config{}, fmt.Errorf("resolve workspace directory: %w", err)
	}
	info, err := os.Stat(workspace)
	if err != nil {
		return Config{}, fmt.Errorf("inspect workspace directory %q: %w", workspace, err)
	}
	if !info.IsDir() {
		return Config{}, fmt.Errorf("workspace directory %q is not a directory", workspace)
	}
	c.WorkspaceDirectory = workspace
	return c, nil
}

// RuntimeDatabasePath stores only local execution resources linked to ATE.
// The legacy history database is preserved separately during the cutover.
func (c Config) RuntimeDatabasePath() string {
	return filepath.Join(c.DataDirectory, "runtime-v3.db")
}

// SessionDatabasePath identifies the legacy history format for explicit import
// and legacy adapter tests. New runtime assembly uses RuntimeDatabasePath.
func (c Config) SessionDatabasePath() string {
	return filepath.Join(c.DataDirectory, "sessions.db")
}

// SessionRegistryPath is the user-level catalog for managed Agent Sessions.
// It is intentionally separate from a Session's conversation database.
func (c Config) SessionRegistryPath() string {
	return filepath.Join(c.HomeDirectory, "control.db")
}

// CoordinationHomeDirectory owns the per-workspace coordination databases, so
// every data directory under one GSEA_HOME coordinates with the others.
// Programmatic configurations without a home coordinate within their data
// directory.
func (c Config) CoordinationHomeDirectory() string {
	if c.HomeDirectory != "" {
		return c.HomeDirectory
	}
	return c.DataDirectory
}
