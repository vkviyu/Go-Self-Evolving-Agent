package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentgoal"
	"github.com/vkviyu/go-self-evolving-agent/internal/lsp"
	pluginwasm "github.com/vkviyu/go-self-evolving-agent/internal/plugin/wasm"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenusage"
)

const (
	configFileName         = "config.toml"
	profileFileSuffix      = ".config.toml"
	projectConfigDirectory = ".gsea"
)

type (
	// LoadOptions controls configuration sources that depend on invocation
	// context. Profile files layer on top of the user file; project config is
	// discovered from WorkingDirectory.
	LoadOptions struct {
		WorkingDirectory string
		Profile          string
		Strict           bool
		// TrustProjectConfig is explicit authority from the embedding host,
		// never a field read from the repository itself.
		TrustProjectConfig bool
	}

	fileConfig struct {
		DataDirectory      *string          `toml:"data_directory"`
		WorkspaceDirectory *string          `toml:"workspace_directory"`
		Model              fileModelConfig  `toml:"model"`
		Agent              fileAgentConfig  `toml:"agent"`
		Agents             fileAgentsConfig `toml:"agents"`
		Goal               fileGoalConfig   `toml:"goal"`
		LSP                fileLSPConfig    `toml:"lsp"`
		WASM               fileWASMConfig   `toml:"wasm"`
		Usage              fileUsageConfig  `toml:"usage"`
	}

	fileModelConfig struct {
		Provider          *string `toml:"provider"`
		WireAPI           *string `toml:"wire_api"`
		Name              *string `toml:"name"`
		TokenizerEncoding *string `toml:"tokenizer_encoding"`
		BaseURL           *string `toml:"base_url"`
		APIKeyEnvironment *string `toml:"api_key_environment"`
		ReasoningEffort   *string `toml:"reasoning_effort"`
		Timeout           *string `toml:"timeout"`
	}

	fileAgentConfig struct {
		Instruction             *string `toml:"instruction"`
		MaxIterations           *int    `toml:"max_iterations"`
		MaxOutputTokens         *int    `toml:"max_output_tokens"`
		ContextWindowTokens     *int    `toml:"context_window_tokens"`
		CompactionTriggerTokens *int    `toml:"compaction_trigger_tokens"`
	}

	fileAgentsConfig struct {
		MaxRunning        *int           `toml:"max_running"`
		MaxLive           *int           `toml:"max_live"`
		MaxDepth          *int           `toml:"max_depth"`
		MaxChildren       *int           `toml:"max_children"`
		ModelLimits       map[string]int `toml:"model_limits"`
		DefaultWorkspace  *string        `toml:"default_workspace"`
		AutoMerge         *bool          `toml:"auto_merge"`
		AutoContinue      *bool          `toml:"auto_continue"`
		SnapshotCopyLimit *string        `toml:"snapshot_copy_limit"`
		// PostMergeCheck is a shell command run in main after each merge.
		PostMergeCheck        *string `toml:"post_merge_check"`
		PostMergeCheckTimeout *string `toml:"post_merge_check_timeout"`
	}

	fileGoalConfig struct {
		Enabled           *bool   `toml:"enabled"`
		Activation        *string `toml:"activation"`
		Assessment        *string `toml:"assessment"`
		MaxRounds         *int    `toml:"max_rounds"`
		MaxWallTime       *string `toml:"max_wall_time"`
		EvaluationTimeout *string `toml:"evaluation_timeout"`
	}

	fileLSPConfig struct {
		Enabled               *bool     `toml:"enabled"`
		Command               *string   `toml:"command"`
		Args                  *[]string `toml:"args"`
		DiagnosticTimeout     *string   `toml:"diagnostic_timeout"`
		MaxDiagnosticsPerFile *int      `toml:"max_diagnostics_per_file"`
		MaxDiagnosticsTotal   *int      `toml:"max_diagnostics_total"`
	}

	fileWASMConfig struct {
		MemoryLimitPages *uint32 `toml:"memory_limit_pages"`
		MaxBlobBytes     *uint64 `toml:"max_blob_bytes"`
		MaxCallBytes     *uint64 `toml:"max_call_bytes"`
		DefaultTimeout   *string `toml:"default_timeout"`
	}

	// fileUsageConfig is the [usage] table: [usage.prices."provider/model"]
	// entries and a [usage.budget] for each session Team.
	fileUsageConfig struct {
		Prices map[string]filePriceConfig `toml:"prices"`
		Budget fileBudgetConfig           `toml:"budget"`
	}

	// filePriceConfig prices one model in micro-units per million tokens.
	filePriceConfig struct {
		InputMicrosPerMillion       *int64 `toml:"input_micros_per_million"`
		CachedInputMicrosPerMillion *int64 `toml:"cached_input_micros_per_million"`
		OutputMicrosPerMillion      *int64 `toml:"output_micros_per_million"`
		ReasoningMicrosPerMillion   *int64 `toml:"reasoning_micros_per_million"`
	}

	fileBudgetConfig struct {
		MaxInputTokens  *int   `toml:"max_input_tokens"`
		MaxOutputTokens *int   `toml:"max_output_tokens"`
		MaxTotalTokens  *int   `toml:"max_total_tokens"`
		MaxCostMicros   *int64 `toml:"max_cost_micros"`
	}
)

// Load resolves configuration in deterministic precedence order:
// built-in defaults, user config, selected profile, project config,
// environment variables, then command-line overrides applied by the caller.
func Load(options LoadOptions) (Config, error) {
	// Resolve GSEA_HOME, falling back to ~/.gsea when it is not set.
	// This returns an absolute path without creating the directory.
	home, err := resolveHomeDirectory()

	// Stop loading if the home directory cannot be resolved.
	// Config{} is the zero value returned alongside the error.
	if err != nil {
		return Config{}, err
	}

	// Create the initial configuration with built-in defaults.
	// All subsequent sources merge their fields into this same value.
	config := builtInConfig(home)

	// Resolve the rendezvous directory from GSEA_COORDINATION_DIR or the user's state directory.
	// Configuration files cannot override it, so every GSEA_HOME of this user agrees on one location.
	config.CoordinationDirectory = resolveCoordinationDirectory()

	// Enable strict parsing if requested by either the caller or the environment.
	// Strict parsing rejects unknown TOML fields.
	// The || operator short-circuits when options.Strict is already true.
	strict := options.Strict || environmentBool("GSEA_STRICT_CONFIG")

	// Load <home>/config.toml and merge its explicit fields into config.
	// &config allows loadConfigFile to modify the existing configuration.
	// required=false means a missing file is allowed.
	// Other read, parse or merge errors stop loading.
	if err := loadConfigFile(filepath.Join(home, configFileName), &config, strict, false); err != nil {
		return Config{}, err
	}

	// Read the caller's profile name and trim surrounding whitespace.
	// An omitted or whitespace-only name becomes an empty string.
	profile := strings.TrimSpace(options.Profile)

	// If the profile name is empty, read it from the GSEA_PROFILE environment variable.
	// If it already has a value, keep that value and skip this environment lookup.
	if profile == "" {
		profile = strings.TrimSpace(os.Getenv(ProfileEnvironment))
	}

	// If the profile name is still empty, skip profile file loading.
	// Otherwise, validate the name and load the corresponding profile file.
	if profile != "" {
		// Reject ".", ".." and path separators.
		// This keeps the profile name usable as a filename component.
		if err := validateProfileName(profile); err != nil {
			return Config{}, err
		}

		// Build <home>/<profile>.config.toml.
		// For example, profile "work" selects ~/.gsea/work.config.toml
		// when GSEA_HOME is not set.
		profilePath := filepath.Join(home, profile+profileFileSuffix)

		// Merge profile fields over the defaults and user configuration.
		// required=true means the selected profile file must exist.
		// strict still controls whether unknown TOML fields are rejected.
		if err := loadConfigFile(profilePath, &config, strict, true); err != nil {
			return Config{}, err
		}
	}

	// Search for the nearest .gsea/config.toml from WorkingDirectory upward.
	// An empty WorkingDirectory uses the process's current working directory.
	// The search returns one file; it does not merge all ancestor configurations.
	// projectPath is the matching path; found indicates whether a match exists.
	projectPath, found, err := findProjectConfig(options.WorkingDirectory)

	// A search error is different from simply finding no project configuration.
	if err != nil {
		return Config{}, err
	}

	// Skip project configuration loading when no matching file was found.
	if found {
		trusted := options.TrustProjectConfig
		if value := strings.TrimSpace(os.Getenv("GSEA_TRUST_PROJECT_CONFIG")); value != "" {
			grant, err := strconv.ParseBool(value)
			if err != nil {
				return Config{}, fmt.Errorf("GSEA_TRUST_PROJECT_CONFIG: %w", err)
			}
			trusted = trusted || grant
		}
		// Discovery can reach the user's own configuration while walking up
		// from a project under HOME. That file was already loaded as trusted.
		projectInfo, projectErr := os.Stat(projectPath)
		userInfo, userErr := os.Stat(filepath.Join(home, configFileName))
		userConfig := projectErr == nil && userErr == nil && os.SameFile(projectInfo, userInfo)
		if !trusted && !userConfig {
			return Config{}, fmt.Errorf("%w: %s; inspect the file, then explicitly set GSEA_TRUST_PROJECT_CONFIG=true for this invocation", ErrProjectConfigUntrusted, projectPath)
		}
		// Merge project fields over the previously loaded configuration.
		// required=false allows a missing file, including one removed after discovery.
		if err := loadConfigFile(projectPath, &config, strict, false); err != nil {
			return Config{}, err
		}
	}

	// Apply supported non-empty environment variables after all configuration files.
	// These values override corresponding fields from user, profile and project files.
	// Invalid boolean, integer or duration values return an error.
	if err := applyEnvironment(&config); err != nil {
		return Config{}, err
	}

	// Check the final merged timeout, iteration, token, child Agent, LSP, Goal and WASM settings.
	// Zero token limits are allowed and defer to automatic model capacity resolution.
	// This does not create resources or verify model/provider availability.
	if err := validate(config); err != nil {
		return Config{}, err
	}

	// Return the merged and validated configuration.
	// The caller may subsequently apply explicit CLI overrides.
	return config, nil
}

// resolveHomeDirectory returns the absolute GSEA_HOME path, falling back to
// .gsea under the user's home directory. It does not create or inspect the directory.
func resolveHomeDirectory() (string, error) {
	home := strings.TrimSpace(os.Getenv(HomeEnvironment))
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve user home directory: %w", err)
		}
		home = filepath.Join(userHome, ".gsea")
	}
	abs, err := filepath.Abs(home)
	if err != nil {
		return "", fmt.Errorf("resolve GSEA home directory: %w", err)
	}
	return abs, nil
}

// resolveCoordinationDirectory returns the absolute rendezvous directory
// shared by every GSEA_HOME of this user, or empty when no user directory
// can be determined. It does not create or inspect the directory.
func resolveCoordinationDirectory() string {
	directory := strings.TrimSpace(os.Getenv(CoordinationEnvironment))
	if directory == "" {
		base, err := userStateDirectory()
		if err != nil {
			return ""
		}
		directory = filepath.Join(base, "gsea", "coordination")
	}
	abs, err := filepath.Abs(directory)
	if err != nil {
		return ""
	}
	return abs
}

// userStateDirectory follows XDG_STATE_HOME on Linux and uses the
// per-user application data directory elsewhere, such as Application
// Support on macOS.
func userStateDirectory() (string, error) {
	if runtime.GOOS != "linux" {
		return os.UserConfigDir()
	}
	if state := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); filepath.IsAbs(state) {
		return state, nil
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(userHome, ".local", "state"), nil
}

// builtInConfig supplies the initial configuration without reading files or
// creating runtime resources. Zero token limits defer to model capacity resolution.
func builtInConfig(home string) Config {
	return Config{
		HomeDirectory: home,
		DataDirectory: home,
		Model: ModelConfig{
			Provider:          "ollama",
			WireAPI:           "chat-completions",
			APIKeyEnvironment: "OPENAI_API_KEY",
			ReasoningEffort:   "medium",
			Timeout:           2 * time.Minute,
		},
		Agent: AgentConfig{
			Instruction:             "You are a local-first coding Agent. Inspect the workspace before changing it, use the provided workspace tools for files and shell commands, respect approval decisions, and report failures honestly.",
			MaxIterations:           20,
			MaxOutputTokens:         0, // auto: reserve output after resolving model capacity
			ContextWindowTokens:     0, // auto: provider metadata, model catalog, fallback
			CompactionTriggerTokens: 0, // auto: working-context policy plus output reserve
		},
		// Read-only children run in parallel up to MaxRunning; children that
		// write the shared workspace still run one at a time.
		Agents: AgentsConfig{
			MaxRunning: 4, MaxLive: 16, MaxDepth: 3, MaxChildren: 4,
			DefaultWorkspace: AgentsWorkspaceAuto, SnapshotCopyLimit: DefaultSnapshotCopyLimit,
			AutoContinue: true, PostMergeCheckTimeout: 10 * time.Minute,
		},
		Goal: agentgoal.Config{
			Enabled: true, Activation: agentgoal.ActivationAuto, Assessment: agentgoal.AssessmentAdaptive,
			MaxRounds: 4, MaxWallTime: 30 * time.Minute, EvaluationTimeout: 30 * time.Second,
		},
		LSP: lsp.Config{
			Enabled:               true,
			Command:               "gopls",
			Args:                  []string{"serve"},
			DiagnosticTimeout:     3 * time.Second,
			MaxDiagnosticsPerFile: 5,
			MaxDiagnosticsTotal:   20,
		},
		WASM: pluginwasm.Config{
			MemoryLimitPages: 1024,
			MaxBlobBytes:     16 << 20,
			MaxCallBytes:     32 << 20,
			DefaultTimeout:   30 * time.Second,
		},
	}
}

// loadConfigFile reads TOML and merges explicitly configured fields into target.
// strict rejects unknown fields; required makes a missing file an error.
// Read, parse and merge errors include the source path.
func loadConfigFile(path string, target *Config, strict, required bool) error {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		if required {
			return fmt.Errorf("profile config %q does not exist", path)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("read config file %q: %w", path, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("inspect config file %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("config file %q must be a regular file", path)
	}
	const maxConfigBytes = 1 << 20
	data, err := io.ReadAll(io.LimitReader(file, maxConfigBytes+1))
	if err != nil {
		return fmt.Errorf("read config file %q: %w", path, err)
	}
	if len(data) > maxConfigBytes {
		return fmt.Errorf("config file %q exceeds %d bytes", path, maxConfigBytes)
	}
	var source fileConfig
	decoder := toml.NewDecoder(bytes.NewReader(data))
	if strict {
		decoder.DisallowUnknownFields()
	}
	if err := decoder.Decode(&source); err != nil {
		return fmt.Errorf("parse config file %q: %w", path, err)
	}
	if err := applyFileConfig(target, source); err != nil {
		return fmt.Errorf("apply config file %q: %w", path, err)
	}
	return nil
}

// applyFileConfig merges non-nil fields, preserving values omitted from source.
// Pointers distinguish omitted fields from explicit false, zero or empty values.
// It parses durations and leaves other checks to validate; errors do not roll back
// assignments already applied to target.
func applyFileConfig(target *Config, source fileConfig) error {
	if source.DataDirectory != nil {
		target.DataDirectory = strings.TrimSpace(*source.DataDirectory)
	}
	if source.WorkspaceDirectory != nil {
		target.WorkspaceDirectory = strings.TrimSpace(*source.WorkspaceDirectory)
	}
	if source.Model.Provider != nil {
		target.Model.Provider = strings.TrimSpace(*source.Model.Provider)
	}
	if source.Model.WireAPI != nil {
		target.Model.WireAPI = strings.TrimSpace(*source.Model.WireAPI)
	}
	if source.Model.Name != nil {
		target.Model.Name = strings.TrimSpace(*source.Model.Name)
	}
	if source.Model.TokenizerEncoding != nil {
		target.Model.TokenizerEncoding = strings.TrimSpace(*source.Model.TokenizerEncoding)
	}
	if source.Model.BaseURL != nil {
		target.Model.BaseURL = strings.TrimSpace(*source.Model.BaseURL)
	}
	if source.Model.APIKeyEnvironment != nil {
		target.Model.APIKeyEnvironment = strings.TrimSpace(*source.Model.APIKeyEnvironment)
	}
	if source.Model.ReasoningEffort != nil {
		target.Model.ReasoningEffort = strings.TrimSpace(*source.Model.ReasoningEffort)
	}
	if source.Model.Timeout != nil {
		duration, err := time.ParseDuration(strings.TrimSpace(*source.Model.Timeout))
		if err != nil || duration <= 0 {
			return fmt.Errorf("model.timeout must be a positive duration")
		}
		target.Model.Timeout = duration
	}
	if source.Agent.Instruction != nil {
		target.Agent.Instruction = *source.Agent.Instruction
	}
	if source.Agent.MaxIterations != nil {
		target.Agent.MaxIterations = *source.Agent.MaxIterations
	}
	if source.Agent.MaxOutputTokens != nil {
		target.Agent.MaxOutputTokens = *source.Agent.MaxOutputTokens
	}
	if source.Agent.ContextWindowTokens != nil {
		target.Agent.ContextWindowTokens = *source.Agent.ContextWindowTokens
	}
	if source.Agent.CompactionTriggerTokens != nil {
		target.Agent.CompactionTriggerTokens = *source.Agent.CompactionTriggerTokens
	}
	if source.Agents.MaxRunning != nil {
		target.Agents.MaxRunning = *source.Agents.MaxRunning
	}
	if source.Agents.MaxLive != nil {
		target.Agents.MaxLive = *source.Agents.MaxLive
	}
	if source.Agents.MaxDepth != nil {
		target.Agents.MaxDepth = *source.Agents.MaxDepth
	}
	if source.Agents.MaxChildren != nil {
		target.Agents.MaxChildren = *source.Agents.MaxChildren
	}
	if len(source.Agents.ModelLimits) > 0 {
		// Later sources override individual models instead of replacing the
		// whole table. Clone so earlier Config values never share the map.
		limits := maps.Clone(target.Agents.ModelLimits)
		if limits == nil {
			limits = make(map[string]int, len(source.Agents.ModelLimits))
		}
		for name, limit := range source.Agents.ModelLimits {
			limits[strings.TrimSpace(name)] = limit
		}
		target.Agents.ModelLimits = limits
	}
	if source.Agents.DefaultWorkspace != nil {
		target.Agents.DefaultWorkspace = strings.ToLower(strings.TrimSpace(*source.Agents.DefaultWorkspace))
	}
	if source.Agents.AutoMerge != nil {
		target.Agents.AutoMerge = *source.Agents.AutoMerge
	}
	if source.Agents.AutoContinue != nil {
		target.Agents.AutoContinue = *source.Agents.AutoContinue
	}
	if source.Agents.SnapshotCopyLimit != nil {
		limit, err := parseByteSize(*source.Agents.SnapshotCopyLimit)
		if err != nil {
			return fmt.Errorf("agents.snapshot_copy_limit: %w", err)
		}
		target.Agents.SnapshotCopyLimit = limit
	}
	if source.Agents.PostMergeCheck != nil {
		target.Agents.PostMergeCheck = strings.TrimSpace(*source.Agents.PostMergeCheck)
	}
	if source.Agents.PostMergeCheckTimeout != nil {
		timeout, err := time.ParseDuration(strings.TrimSpace(*source.Agents.PostMergeCheckTimeout))
		if err != nil {
			return fmt.Errorf("agents.post_merge_check_timeout: %w", err)
		}
		target.Agents.PostMergeCheckTimeout = timeout
	}
	if source.Goal.Enabled != nil {
		target.Goal.Enabled = *source.Goal.Enabled
	}
	if source.Goal.Activation != nil {
		target.Goal.Activation = agentgoal.Activation(strings.TrimSpace(*source.Goal.Activation))
	}
	if source.Goal.Assessment != nil {
		target.Goal.Assessment = agentgoal.AssessmentStrategy(strings.TrimSpace(*source.Goal.Assessment))
	}
	if source.Goal.MaxRounds != nil {
		target.Goal.MaxRounds = *source.Goal.MaxRounds
	}
	if source.Goal.EvaluationTimeout != nil {
		duration, err := time.ParseDuration(strings.TrimSpace(*source.Goal.EvaluationTimeout))
		if err != nil || duration <= 0 {
			return errors.New("goal.evaluation_timeout must be a positive duration")
		}
		target.Goal.EvaluationTimeout = duration
	}
	if source.Goal.MaxWallTime != nil {
		duration, err := time.ParseDuration(strings.TrimSpace(*source.Goal.MaxWallTime))
		if err != nil || duration <= 0 {
			return errors.New("goal.max_wall_time must be a positive duration")
		}
		target.Goal.MaxWallTime = duration
	}
	if source.LSP.Enabled != nil {
		target.LSP.Enabled = *source.LSP.Enabled
	}
	if source.LSP.Command != nil {
		target.LSP.Command = strings.TrimSpace(*source.LSP.Command)
	}
	if source.LSP.Args != nil {
		target.LSP.Args = append([]string(nil), (*source.LSP.Args)...)
	}
	if source.LSP.DiagnosticTimeout != nil {
		duration, err := time.ParseDuration(strings.TrimSpace(*source.LSP.DiagnosticTimeout))
		if err != nil || duration <= 0 {
			return errors.New("lsp.diagnostic_timeout must be a positive duration")
		}
		target.LSP.DiagnosticTimeout = duration
	}
	if source.LSP.MaxDiagnosticsPerFile != nil {
		target.LSP.MaxDiagnosticsPerFile = *source.LSP.MaxDiagnosticsPerFile
	}
	if source.LSP.MaxDiagnosticsTotal != nil {
		target.LSP.MaxDiagnosticsTotal = *source.LSP.MaxDiagnosticsTotal
	}
	if source.WASM.MemoryLimitPages != nil {
		target.WASM.MemoryLimitPages = *source.WASM.MemoryLimitPages
	}
	if source.WASM.MaxBlobBytes != nil {
		target.WASM.MaxBlobBytes = *source.WASM.MaxBlobBytes
	}
	if source.WASM.MaxCallBytes != nil {
		target.WASM.MaxCallBytes = *source.WASM.MaxCallBytes
	}
	if source.WASM.DefaultTimeout != nil {
		duration, err := time.ParseDuration(strings.TrimSpace(*source.WASM.DefaultTimeout))
		if err != nil || duration <= 0 {
			return fmt.Errorf("wasm.default_timeout must be a positive duration")
		}
		target.WASM.DefaultTimeout = duration
	}
	applyUsageConfig(&target.Usage, source.Usage)
	return nil
}

// applyUsageConfig merges the [usage] table. Like model_limits, later sources
// override individual price fields of a model instead of replacing the table.
func applyUsageConfig(target *UsageConfig, source fileUsageConfig) {
	if len(source.Prices) > 0 {
		prices := maps.Clone(target.Prices)
		if prices == nil {
			prices = make(map[string]tokenusage.Price, len(source.Prices))
		}
		for key, entry := range source.Prices {
			key = strings.TrimSpace(key)
			price := prices[key]
			setIfPresent(&price.InputMicrosPerMillion, entry.InputMicrosPerMillion)
			setIfPresent(&price.CachedInputMicrosPerMillion, entry.CachedInputMicrosPerMillion)
			setIfPresent(&price.OutputMicrosPerMillion, entry.OutputMicrosPerMillion)
			setIfPresent(&price.ReasoningMicrosPerMillion, entry.ReasoningMicrosPerMillion)
			prices[key] = price
		}
		target.Prices = prices
	}
	setIfPresent(&target.Budget.MaxInputTokens, source.Budget.MaxInputTokens)
	setIfPresent(&target.Budget.MaxOutputTokens, source.Budget.MaxOutputTokens)
	setIfPresent(&target.Budget.MaxTotalTokens, source.Budget.MaxTotalTokens)
	setIfPresent(&target.Budget.MaxCostMicros, source.Budget.MaxCostMicros)
}

// setIfPresent copies a configured value over destination.
func setIfPresent[T any](destination *T, value *T) {
	if value != nil {
		*destination = *value
	}
}

// applyEnvironment overrides file configuration with non-empty environment values
// and parses booleans, integers and durations. GSEA_MODEL and GSEA_MODEL_BASE_URL
// take precedence over provider-specific variables. Zero token limits select
// automatic resolution; errors do not roll back prior assignments.
func applyEnvironment(target *Config) error {
	setString := func(name string, destination *string) {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			*destination = value
		}
	}
	setString("GSEA_DATA_DIRECTORY", &target.DataDirectory)
	setString("GSEA_WORKSPACE_DIRECTORY", &target.WorkspaceDirectory)
	setString("GSEA_MODEL_PROVIDER", &target.Model.Provider)
	setString("GSEA_MODEL_WIRE_API", &target.Model.WireAPI)
	setString("GSEA_MODEL", &target.Model.Name)
	setString("GSEA_TOKENIZER_ENCODING", &target.Model.TokenizerEncoding)
	setString("GSEA_MODEL_BASE_URL", &target.Model.BaseURL)
	setString("GSEA_API_KEY_ENV", &target.Model.APIKeyEnvironment)
	setString("GSEA_REASONING_EFFORT", &target.Model.ReasoningEffort)
	setString("GSEA_AGENT_INSTRUCTION", &target.Agent.Instruction)
	setString("GSEA_LSP_COMMAND", &target.LSP.Command)
	if value := strings.TrimSpace(os.Getenv("GSEA_LSP_ENABLED")); value != "" {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return errors.New("GSEA_LSP_ENABLED must be a boolean")
		}
		target.LSP.Enabled = parsed
	}
	if value := strings.TrimSpace(os.Getenv("GSEA_GOAL_ENABLED")); value != "" {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return errors.New("GSEA_GOAL_ENABLED must be a boolean")
		}
		target.Goal.Enabled = parsed
	}
	if value := strings.TrimSpace(os.Getenv("GSEA_GOAL_ASSESSMENT")); value != "" {
		target.Goal.Assessment = agentgoal.AssessmentStrategy(value)
	}
	if value := strings.TrimSpace(os.Getenv("GSEA_GOAL_ACTIVATION")); value != "" {
		target.Goal.Activation = agentgoal.Activation(value)
	}
	if value := strings.TrimSpace(os.Getenv("GSEA_GOAL_EVALUATION_TIMEOUT")); value != "" {
		duration, err := time.ParseDuration(value)
		if err != nil || duration <= 0 {
			return errors.New("GSEA_GOAL_EVALUATION_TIMEOUT must be a positive duration")
		}
		target.Goal.EvaluationTimeout = duration
	}
	if value := strings.TrimSpace(os.Getenv("GSEA_GOAL_MAX_ROUNDS")); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed <= 0 {
			return errors.New("GSEA_GOAL_MAX_ROUNDS must be a positive integer")
		}
		target.Goal.MaxRounds = parsed
	}
	if value := strings.TrimSpace(os.Getenv("GSEA_GOAL_MAX_WALL_TIME")); value != "" {
		duration, err := time.ParseDuration(value)
		if err != nil || duration <= 0 {
			return errors.New("GSEA_GOAL_MAX_WALL_TIME must be a positive duration")
		}
		target.Goal.MaxWallTime = duration
	}
	if value := strings.TrimSpace(os.Getenv("GSEA_LSP_DIAGNOSTIC_TIMEOUT")); value != "" {
		duration, err := time.ParseDuration(value)
		if err != nil || duration <= 0 {
			return errors.New("GSEA_LSP_DIAGNOSTIC_TIMEOUT must be a positive duration")
		}
		target.LSP.DiagnosticTimeout = duration
	}
	if value := strings.TrimSpace(os.Getenv("GSEA_LSP_MAX_DIAGNOSTICS_PER_FILE")); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed <= 0 {
			return errors.New("GSEA_LSP_MAX_DIAGNOSTICS_PER_FILE must be a positive integer")
		}
		target.LSP.MaxDiagnosticsPerFile = parsed
	}
	if value := strings.TrimSpace(os.Getenv("GSEA_LSP_MAX_DIAGNOSTICS_TOTAL")); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed <= 0 {
			return errors.New("GSEA_LSP_MAX_DIAGNOSTICS_TOTAL must be a positive integer")
		}
		target.LSP.MaxDiagnosticsTotal = parsed
	}
	if value := strings.TrimSpace(os.Getenv("GSEA_TIMEOUT")); value != "" {
		duration, err := time.ParseDuration(value)
		if err != nil || duration <= 0 {
			return errors.New("GSEA_TIMEOUT must be a positive duration")
		}
		target.Model.Timeout = duration
	}
	if value := strings.TrimSpace(os.Getenv("GSEA_MAX_ITERATIONS")); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed <= 0 {
			return errors.New("GSEA_MAX_ITERATIONS must be a positive integer")
		}
		target.Agent.MaxIterations = parsed
	}
	if value := strings.TrimSpace(os.Getenv("GSEA_MAX_OUTPUT_TOKENS")); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 0 {
			return errors.New("GSEA_MAX_OUTPUT_TOKENS must be a non-negative integer (0 = auto)")
		}
		target.Agent.MaxOutputTokens = parsed
	}
	if value := strings.TrimSpace(os.Getenv("GSEA_CONTEXT_WINDOW_TOKENS")); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 0 {
			return errors.New("GSEA_CONTEXT_WINDOW_TOKENS must be a non-negative integer (0 = auto)")
		}
		target.Agent.ContextWindowTokens = parsed
	}
	if value := strings.TrimSpace(os.Getenv("GSEA_COMPACTION_TRIGGER_TOKENS")); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 0 {
			return errors.New("GSEA_COMPACTION_TRIGGER_TOKENS must be a non-negative integer (0 = auto)")
		}
		target.Agent.CompactionTriggerTokens = parsed
	}
	for name, destination := range map[string]*int{
		"GSEA_AGENTS_MAX_RUNNING":  &target.Agents.MaxRunning,
		"GSEA_AGENTS_MAX_LIVE":     &target.Agents.MaxLive,
		"GSEA_AGENTS_MAX_DEPTH":    &target.Agents.MaxDepth,
		"GSEA_AGENTS_MAX_CHILDREN": &target.Agents.MaxChildren,
	} {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			parsed, err := strconv.Atoi(value)
			if err != nil || parsed <= 0 {
				return fmt.Errorf("%s must be a positive integer", name)
			}
			*destination = parsed
		}
	}
	if value := strings.TrimSpace(os.Getenv("GSEA_AGENTS_DEFAULT_WORKSPACE")); value != "" {
		target.Agents.DefaultWorkspace = strings.ToLower(value)
	}
	if value := strings.TrimSpace(os.Getenv("GSEA_AGENTS_AUTO_MERGE")); value != "" {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return errors.New("GSEA_AGENTS_AUTO_MERGE must be a boolean")
		}
		target.Agents.AutoMerge = parsed
	}
	if value := strings.TrimSpace(os.Getenv("GSEA_AGENTS_AUTO_CONTINUE")); value != "" {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return errors.New("GSEA_AGENTS_AUTO_CONTINUE must be a boolean")
		}
		target.Agents.AutoContinue = parsed
	}
	if value := strings.TrimSpace(os.Getenv("GSEA_AGENTS_SNAPSHOT_COPY_LIMIT")); value != "" {
		limit, err := parseByteSize(value)
		if err != nil {
			return fmt.Errorf("GSEA_AGENTS_SNAPSHOT_COPY_LIMIT: %w", err)
		}
		target.Agents.SnapshotCopyLimit = limit
	}
	if value := strings.TrimSpace(os.Getenv("GSEA_AGENTS_POST_MERGE_CHECK")); value != "" {
		target.Agents.PostMergeCheck = value
	}
	if value := strings.TrimSpace(os.Getenv("GSEA_AGENTS_POST_MERGE_CHECK_TIMEOUT")); value != "" {
		timeout, err := time.ParseDuration(value)
		if err != nil {
			return errors.New("GSEA_AGENTS_POST_MERGE_CHECK_TIMEOUT must be a duration")
		}
		target.Agents.PostMergeCheckTimeout = timeout
	}
	provider := strings.ToLower(strings.TrimSpace(target.Model.Provider))
	switch provider {
	case "openai":
		if strings.TrimSpace(os.Getenv("GSEA_MODEL")) == "" {
			setString("OPENAI_MODEL", &target.Model.Name)
		}
		if strings.TrimSpace(os.Getenv("GSEA_MODEL_BASE_URL")) == "" {
			setString("OPENAI_BASE_URL", &target.Model.BaseURL)
		}
	case "ollama":
		if strings.TrimSpace(os.Getenv("GSEA_MODEL")) == "" {
			setString("OLLAMA_MODEL", &target.Model.Name)
		}
		if strings.TrimSpace(os.Getenv("GSEA_MODEL_BASE_URL")) == "" {
			setString("OLLAMA_HOST", &target.Model.BaseURL)
		}
	}
	return nil
}

// findProjectConfig searches ancestors for the nearest .gsea/config.toml,
// returning one match without merging ancestors or stopping at Git boundaries.
// An empty start uses the current directory; an existing non-directory starts
// the search at its parent. Reaching the filesystem root returns found=false;
// candidate inspection errors other than missing paths are returned.
func findProjectConfig(workingDirectory string) (string, bool, error) {
	if strings.TrimSpace(workingDirectory) == "" {
		var err error
		workingDirectory, err = os.Getwd()
		if err != nil {
			return "", false, fmt.Errorf("resolve working directory: %w", err)
		}
	}
	directory, err := filepath.Abs(workingDirectory)
	if err != nil {
		return "", false, fmt.Errorf("resolve project directory: %w", err)
	}
	if info, err := os.Stat(directory); err == nil && !info.IsDir() {
		directory = filepath.Dir(directory)
	}
	userHome, _ := os.UserHomeDir()
	userHome = filepath.Clean(userHome)
	for {
		// HOME/.gsea is a user configuration, not repository configuration.
		// In particular a GSEA_HOME override must not reload the real user's
		// unrelated config while walking out of the working tree.
		if directory == userHome {
			return "", false, nil
		}
		candidate := filepath.Join(directory, projectConfigDirectory, configFileName)
		if _, err := os.Stat(candidate); err == nil {
			return candidate, true, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", false, fmt.Errorf("inspect project config %q: %w", candidate, err)
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return "", false, nil
		}
		directory = parent
	}
}

// validateProfileName rejects ".", ".." and path separators so the name can be
// used as a filename component. Callers handle empty values and whitespace.
// It does not check file existence or require an alphanumeric character set.
func validateProfileName(profile string) error {
	if profile == "." || profile == ".." || filepath.Base(profile) != profile || strings.ContainsAny(profile, `/\\`) {
		return fmt.Errorf("invalid config profile %q", profile)
	}
	return nil
}

// validate checks merged timeout, iteration, token, child Agent, LSP, Goal, WASM and usage settings.
// Zero token limits select automatic resolution; an explicit context window must
// exceed the compaction trigger. It does not create resources, inspect directories
// or verify model names and provider availability.
func validate(config Config) error {
	if config.Model.Timeout <= 0 {
		return errors.New("model timeout must be positive")
	}
	if config.Agent.MaxIterations <= 0 {
		return errors.New("agent.max_iterations must be positive")
	}
	if config.Agent.MaxOutputTokens < 0 {
		return errors.New("agent.max_output_tokens must be non-negative (0 = auto)")
	}
	if config.Agent.ContextWindowTokens < 0 {
		return errors.New("agent.context_window_tokens must be non-negative (0 = auto)")
	}
	if config.Agent.CompactionTriggerTokens < 0 || (config.Agent.ContextWindowTokens > 0 && config.Agent.CompactionTriggerTokens >= config.Agent.ContextWindowTokens) {
		return errors.New("agent.compaction_trigger_tokens must be non-negative (0 = auto) and below an explicitly configured context window")
	}
	if config.Agents.MaxRunning <= 0 || config.Agents.MaxLive <= 0 || config.Agents.MaxDepth <= 0 || config.Agents.MaxChildren <= 0 {
		return errors.New("agents.max_running, max_live, max_depth and max_children must be positive")
	}
	for name, limit := range config.Agents.ModelLimits {
		if name == "" || limit <= 0 {
			return fmt.Errorf("agents.model_limits entry %q must name a model and have a positive limit", name)
		}
	}
	switch config.Agents.DefaultWorkspace {
	case AgentsWorkspaceAuto, AgentsWorkspaceSnapshot, AgentsWorkspaceReadonly, AgentsWorkspaceShared:
	default:
		return fmt.Errorf("agents.default_workspace %q must be auto, snapshot, readonly or shared", config.Agents.DefaultWorkspace)
	}
	if config.Agents.SnapshotCopyLimit <= 0 {
		return errors.New("agents.snapshot_copy_limit must be positive")
	}
	if config.Agents.PostMergeCheckTimeout <= 0 {
		return errors.New("agents.post_merge_check_timeout must be positive")
	}
	if err := config.LSP.Validate(); err != nil {
		return err
	}
	if err := config.Goal.Validate(); err != nil {
		return err
	}
	if config.WASM.MemoryLimitPages == 0 || config.WASM.MaxBlobBytes == 0 || config.WASM.MaxCallBytes == 0 || config.WASM.DefaultTimeout <= 0 {
		return errors.New("wasm limits and timeout must be positive")
	}
	for key, price := range config.Usage.Prices {
		if key == "" || price.InputMicrosPerMillion < 0 || price.CachedInputMicrosPerMillion < 0 || price.OutputMicrosPerMillion < 0 || price.ReasoningMicrosPerMillion < 0 {
			return fmt.Errorf("usage.prices entry %q must name a model and have non-negative prices", key)
		}
	}
	budget := config.Usage.Budget
	if budget.MaxInputTokens < 0 || budget.MaxOutputTokens < 0 || budget.MaxTotalTokens < 0 || budget.MaxCostMicros < 0 {
		return errors.New("usage.budget limits must be non-negative (0 = unlimited)")
	}
	return nil
}

// environmentBool reads a boolean using strconv.ParseBool.
// Unset, blank and invalid values return false without propagating parse errors.
func environmentBool(name string) bool {
	value, err := strconv.ParseBool(strings.TrimSpace(os.Getenv(name)))
	return err == nil && value
}

// byteUnits maps size suffixes to their multipliers. Decimal and binary
// suffixes are distinct; a bare number counts bytes.
var byteUnits = map[string]int64{
	"": 1, "b": 1,
	"kb": 1000, "mb": 1000 * 1000, "gb": 1000 * 1000 * 1000, "tb": 1000 * 1000 * 1000 * 1000,
	"kib": 1 << 10, "mib": 1 << 20, "gib": 1 << 30, "tib": 1 << 40,
}

// parseByteSize parses a positive size such as "1GiB", "512MB" or "4096".
func parseByteSize(value string) (int64, error) {
	value = strings.TrimSpace(value)
	split := len(value)
	for split > 0 && (value[split-1] < '0' || value[split-1] > '9') {
		split--
	}
	number, unit := strings.TrimSpace(value[:split]), strings.ToLower(strings.TrimSpace(value[split:]))
	multiplier, ok := byteUnits[unit]
	parsed, err := strconv.ParseInt(number, 10, 64)
	if !ok || err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%q must be a positive size such as 1GiB or 512MB", value)
	}
	if parsed > (1<<63-1)/multiplier {
		return 0, fmt.Errorf("%q is too large", value)
	}
	return parsed * multiplier, nil
}
