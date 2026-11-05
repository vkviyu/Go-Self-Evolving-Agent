package config

import (
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentgoal"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenusage"
)

func TestGoalAssessmentTimeoutConfiguration(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	t.Setenv(HomeEnvironment, home)
	t.Setenv(ProfileEnvironment, "")
	t.Setenv("GSEA_GOAL_ENABLED", "true")
	t.Setenv("GSEA_GOAL_ASSESSMENT", "")
	t.Setenv("GSEA_GOAL_EVALUATION_TIMEOUT", "")
	loaded, err := Load(LoadOptions{WorkingDirectory: project})
	if err != nil || loaded.Goal.Assessment != agentgoal.AssessmentAdaptive || loaded.Goal.EvaluationTimeout != 30*time.Second {
		t.Fatalf("defaults=%+v err=%v", loaded.Goal, err)
	}
	writeConfig(t, filepath.Join(home, configFileName), "[goal]\nassessment = \"self\"\nevaluation_timeout = \"12s\"\n")
	loaded, err = Load(LoadOptions{WorkingDirectory: project})
	if err != nil || loaded.Goal.Assessment != agentgoal.AssessmentSelf || loaded.Goal.EvaluationTimeout != 12*time.Second {
		t.Fatalf("file=%+v err=%v", loaded.Goal, err)
	}
	t.Setenv("GSEA_GOAL_EVALUATION_TIMEOUT", "4s")
	loaded, err = Load(LoadOptions{WorkingDirectory: project})
	if err != nil || loaded.Goal.EvaluationTimeout != 4*time.Second {
		t.Fatalf("env=%+v err=%v", loaded.Goal, err)
	}
	t.Setenv("GSEA_GOAL_EVALUATION_TIMEOUT", "0s")
	if _, err := Load(LoadOptions{WorkingDirectory: project}); err == nil {
		t.Fatal("zero configured assessment timeout accepted")
	}
}

func TestGoalActivationConfigurationPrecedence(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	t.Setenv(HomeEnvironment, home)
	t.Setenv(ProfileEnvironment, "")
	t.Setenv("GSEA_GOAL_ENABLED", "true")
	t.Setenv("GSEA_GOAL_ACTIVATION", "")
	loaded, err := Load(LoadOptions{WorkingDirectory: project})
	if err != nil || loaded.Goal.Activation != agentgoal.ActivationAuto {
		t.Fatalf("default activation=%q err=%v", loaded.Goal.Activation, err)
	}
	writeConfig(t, filepath.Join(home, configFileName), "[goal]\nactivation = \"always\"\n")
	loaded, err = Load(LoadOptions{WorkingDirectory: project})
	if err != nil || loaded.Goal.Activation != agentgoal.ActivationAlways {
		t.Fatalf("file activation=%q err=%v", loaded.Goal.Activation, err)
	}
	t.Setenv("GSEA_GOAL_ACTIVATION", "auto")
	loaded, err = Load(LoadOptions{WorkingDirectory: project})
	if err != nil || loaded.Goal.Activation != agentgoal.ActivationAuto {
		t.Fatalf("environment activation=%q err=%v", loaded.Goal.Activation, err)
	}
	t.Setenv("GSEA_GOAL_ACTIVATION", "invalid")
	if _, err := Load(LoadOptions{WorkingDirectory: project}); err == nil {
		t.Fatal("invalid activation accepted")
	}
}

func TestDefaultSelectsProviderSpecificEnvironment(t *testing.T) {
	directory := t.TempDir()
	t.Setenv(HomeEnvironment, directory)
	t.Setenv("GSEA_MODEL_PROVIDER", "OPENAI")
	t.Setenv("GSEA_MODEL", "")
	t.Setenv("GSEA_MODEL_BASE_URL", "")
	t.Setenv("OPENAI_MODEL", "test-openai-model")
	t.Setenv("OPENAI_BASE_URL", "https://model.test/v1")
	t.Setenv("GSEA_REASONING_EFFORT", "")
	t.Setenv("OLLAMA_MODEL", "must-not-be-selected")
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:11434")

	// Do not inherit a developer's ancestor project configuration.
	loaded, err := Load(LoadOptions{WorkingDirectory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if loaded.HomeDirectory != directory {
		t.Fatalf("home directory = %q, want %q", loaded.HomeDirectory, directory)
	}
	if loaded.Model.Name != "test-openai-model" || loaded.Model.BaseURL != "https://model.test/v1" || loaded.Model.WireAPI != "chat-completions" || loaded.Model.ReasoningEffort != "medium" {
		t.Fatalf("unexpected model config: %#v", loaded.Model)
	}
	resolved, err := loaded.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if resolved.SessionDatabasePath() != filepath.Join(directory, "sessions.db") {
		t.Fatalf("database path = %q", resolved.SessionDatabasePath())
	}
}

func TestDefaultFallsBackToUserHome(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	t.Setenv(HomeEnvironment, "")
	t.Setenv("GSEA_WORKSPACE_DIRECTORY", "")

	loaded, err := Load(LoadOptions{WorkingDirectory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(userHome, ".gsea")
	if loaded.HomeDirectory != want || loaded.DataDirectory != want {
		t.Fatalf("directories = %q, %q; want %q", loaded.HomeDirectory, loaded.DataDirectory, want)
	}
	if loaded.WorkspaceDirectory != "" {
		t.Fatalf("workspace directory should resolve at application startup, got %q", loaded.WorkspaceDirectory)
	}
}

func TestCoordinationDirectoryIsIndependentOfHome(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv(CoordinationEnvironment, "")
	var resolved []string
	for _, home := range []string{t.TempDir(), t.TempDir()} {
		t.Setenv(HomeEnvironment, home)
		loaded, err := Load(LoadOptions{WorkingDirectory: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		if !filepath.IsAbs(loaded.CoordinationDirectory) || !strings.HasPrefix(loaded.CoordinationDirectory, userHome+string(filepath.Separator)) {
			t.Fatalf("coordination directory %q is not under the user home %q", loaded.CoordinationDirectory, userHome)
		}
		resolved = append(resolved, loaded.CoordinationDirectory)
	}
	if resolved[0] != resolved[1] {
		t.Fatalf("homes resolved different coordination directories: %q", resolved)
	}
	override := t.TempDir()
	t.Setenv(CoordinationEnvironment, override)
	loaded, err := Load(LoadOptions{WorkingDirectory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if loaded.CoordinationDirectory != override {
		t.Fatalf("coordination directory = %q, want override %q", loaded.CoordinationDirectory, override)
	}
}

func TestCoordinationHomeFallsBackToDataDirectory(t *testing.T) {
	if got := (Config{HomeDirectory: "/home", DataDirectory: "/data"}).CoordinationHomeDirectory(); got != "/home" {
		t.Fatalf("coordination home = %q, want GSEA_HOME", got)
	}
	if got := (Config{DataDirectory: "/data"}).CoordinationHomeDirectory(); got != "/data" {
		t.Fatalf("coordination home = %q, want data directory", got)
	}
}

func TestWorkspaceDirectoryPrecedence(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	environmentWorkspace := t.TempDir()
	fileWorkspace := t.TempDir()
	t.Setenv(HomeEnvironment, home)
	t.Setenv("GSEA_WORKSPACE_DIRECTORY", environmentWorkspace)
	writeConfig(t, filepath.Join(home, configFileName), "workspace_directory = \""+fileWorkspace+"\"\n")
	loaded, err := Load(LoadOptions{WorkingDirectory: project})
	if err != nil {
		t.Fatal(err)
	}
	if loaded.WorkspaceDirectory != environmentWorkspace {
		t.Fatalf("workspace directory = %q, want %q", loaded.WorkspaceDirectory, environmentWorkspace)
	}
	loaded.WorkspaceDirectory = project
	resolved, err := loaded.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if resolved.WorkspaceDirectory != project {
		t.Fatalf("resolved workspace directory = %q, want %q", resolved.WorkspaceDirectory, project)
	}
}

func TestLoadPrecedence(t *testing.T) {
	t.Setenv("GSEA_TRUST_PROJECT_CONFIG", "true")
	home := t.TempDir()
	project := t.TempDir()
	t.Setenv(HomeEnvironment, home)
	t.Setenv("GSEA_MODEL_PROVIDER", "openai")
	t.Setenv("GSEA_MODEL", "environment-model")
	t.Setenv("GSEA_MODEL_BASE_URL", "https://environment.test/v1")
	t.Setenv("GSEA_MAX_ITERATIONS", "4")
	t.Setenv("OPENAI_MODEL", "fallback-model")
	t.Setenv("OPENAI_BASE_URL", "https://fallback.test/v1")

	writeConfig(t, filepath.Join(home, configFileName), `
data_directory = "/from-user"
[model]
provider = "openai"
name = "user-model"
timeout = "1s"
[agent]
max_iterations = 1
`)
	writeConfig(t, filepath.Join(home, "work"+profileFileSuffix), `
[model]
name = "profile-model"
base_url = "https://profile.test/v1"
[agent]
max_iterations = 2
`)
	writeConfig(t, filepath.Join(project, projectConfigDirectory, configFileName), `
[model]
name = "project-model"
reasoning_effort = "high"
[agent]
max_iterations = 3
`)
	nested := filepath.Join(project, "src", "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(LoadOptions{WorkingDirectory: nested, Profile: "work"})
	if err != nil {
		t.Fatal(err)
	}
	if loaded.DataDirectory != "/from-user" {
		t.Fatalf("data directory = %q", loaded.DataDirectory)
	}
	if loaded.Model.Provider != "openai" || loaded.Model.Name != "environment-model" || loaded.Model.BaseURL != "https://environment.test/v1" || loaded.Model.ReasoningEffort != "high" || loaded.Model.Timeout.String() != "1s" {
		t.Fatalf("model = %#v", loaded.Model)
	}
	if loaded.Agent.MaxIterations != 4 {
		t.Fatalf("max iterations = %d", loaded.Agent.MaxIterations)
	}
}

func TestLoadUsesProfileEnvironment(t *testing.T) {
	home := t.TempDir()
	t.Setenv(HomeEnvironment, home)
	t.Setenv(ProfileEnvironment, "fast")
	writeConfig(t, filepath.Join(home, "fast"+profileFileSuffix), `[agent]
max_iterations = 7
`)
	loaded, err := Load(LoadOptions{WorkingDirectory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Agent.MaxIterations != 7 {
		t.Fatalf("max iterations = %d", loaded.Agent.MaxIterations)
	}
}

func TestLoadRejectsMissingOrUnsafeProfile(t *testing.T) {
	t.Setenv(HomeEnvironment, t.TempDir())
	for _, profile := range []string{"missing", "../escape", "a/b", ".."} {
		_, err := Load(LoadOptions{Profile: profile})
		if err == nil {
			t.Fatalf("profile %q: expected error", profile)
		}
	}
}

func TestLoadRejectsInvalidEnvironmentValues(t *testing.T) {
	t.Setenv(HomeEnvironment, t.TempDir())
	t.Setenv("GSEA_TIMEOUT", "not-a-duration")
	if _, err := Load(LoadOptions{}); err == nil || !strings.Contains(err.Error(), "GSEA_TIMEOUT") {
		t.Fatalf("timeout error = %v", err)
	}

	t.Setenv("GSEA_TIMEOUT", "")
	t.Setenv("GSEA_MAX_ITERATIONS", "0")
	if _, err := Load(LoadOptions{}); err == nil || !strings.Contains(err.Error(), "GSEA_MAX_ITERATIONS") {
		t.Fatalf("iterations error = %v", err)
	}
}

func TestLoadStrictConfigRejectsUnknownFields(t *testing.T) {
	home := t.TempDir()
	t.Setenv(HomeEnvironment, home)
	t.Setenv("GSEA_STRICT_CONFIG", "true")
	writeConfig(t, filepath.Join(home, configFileName), `unknown = "value"
`)
	if _, err := Load(LoadOptions{}); err == nil || !strings.Contains(err.Error(), "parse config file") {
		t.Fatalf("strict config error = %v", err)
	}
}

func TestLoadLanguageServerConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv(HomeEnvironment, home)
	writeConfig(t, filepath.Join(home, configFileName), `[lsp]
enabled = true
command = "custom-gopls"
args = ["serve", "-debug=localhost:0"]
diagnostic_timeout = "7s"
max_diagnostics_per_file = 3
max_diagnostics_total = 9
`)
	loaded, err := Load(LoadOptions{WorkingDirectory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.LSP.Enabled || loaded.LSP.Command != "custom-gopls" || len(loaded.LSP.Args) != 2 || loaded.LSP.DiagnosticTimeout.String() != "7s" || loaded.LSP.MaxDiagnosticsPerFile != 3 || loaded.LSP.MaxDiagnosticsTotal != 9 {
		t.Fatalf("LSP config = %#v", loaded.LSP)
	}
}

func TestResolveSecuresDataDirectory(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	resolved, err := (Config{DataDirectory: directory}).Resolve()
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(resolved.DataDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if permissions := info.Mode().Perm(); permissions != 0o700 {
		t.Fatalf("data directory permissions = %o, want 700", permissions)
	}
}

func TestAgentsConfigLayersFilesAndEnvironment(t *testing.T) {
	t.Setenv("GSEA_TRUST_PROJECT_CONFIG", "true")
	home := t.TempDir()
	project := t.TempDir()
	t.Setenv(HomeEnvironment, home)
	t.Setenv(ProfileEnvironment, "")
	t.Setenv("GSEA_AGENTS_MAX_RUNNING", "")
	t.Setenv("GSEA_AGENTS_MAX_LIVE", "")
	t.Setenv("GSEA_AGENTS_MAX_DEPTH", "")
	t.Setenv("GSEA_AGENTS_MAX_CHILDREN", "")
	defaults, err := Load(LoadOptions{WorkingDirectory: project})
	if err != nil {
		t.Fatal(err)
	}
	if defaults.Agents.MaxRunning != 4 || defaults.Agents.MaxLive != 16 || defaults.Agents.MaxDepth != 3 || defaults.Agents.MaxChildren != 4 || defaults.Agents.ModelLimits != nil {
		t.Fatalf("default child Agent limits = %#v", defaults.Agents)
	}
	writeConfig(t, filepath.Join(home, configFileName), `
[agents]
max_running = 2
max_depth = 2
[agents.model_limits]
gpt-large = 1
gpt-small = 3
`)
	writeConfig(t, filepath.Join(project, projectConfigDirectory, configFileName), `
[agents]
max_children = 6
[agents.model_limits]
gpt-small = 2
`)
	t.Setenv("GSEA_AGENTS_MAX_RUNNING", "5")
	loaded, err := Load(LoadOptions{WorkingDirectory: project})
	if err != nil {
		t.Fatal(err)
	}
	want := AgentsConfig{MaxRunning: 5, MaxLive: 16, MaxDepth: 2, MaxChildren: 6, ModelLimits: map[string]int{"gpt-large": 1, "gpt-small": 2}}
	if loaded.Agents.MaxRunning != want.MaxRunning || loaded.Agents.MaxLive != want.MaxLive || loaded.Agents.MaxDepth != want.MaxDepth || loaded.Agents.MaxChildren != want.MaxChildren || !maps.Equal(loaded.Agents.ModelLimits, want.ModelLimits) {
		t.Fatalf("child Agent limits = %#v, want %#v", loaded.Agents, want)
	}
	t.Setenv("GSEA_AGENTS_MAX_RUNNING", "0")
	if _, err := Load(LoadOptions{WorkingDirectory: project}); err == nil || !strings.Contains(err.Error(), "GSEA_AGENTS_MAX_RUNNING") {
		t.Fatalf("zero running limit error = %v", err)
	}
	t.Setenv("GSEA_AGENTS_MAX_RUNNING", "")
	writeConfig(t, filepath.Join(project, projectConfigDirectory, configFileName), "[agents.model_limits]\ngpt-small = 0\n")
	if _, err := Load(LoadOptions{WorkingDirectory: project}); err == nil || !strings.Contains(err.Error(), "model_limits") {
		t.Fatalf("zero model limit error = %v", err)
	}
}

func TestAgentsWorkspaceConfigLayersFilesAndEnvironment(t *testing.T) {
	t.Setenv("GSEA_TRUST_PROJECT_CONFIG", "true")
	home := t.TempDir()
	project := t.TempDir()
	t.Setenv(HomeEnvironment, home)
	t.Setenv(ProfileEnvironment, "")
	for _, name := range []string{"GSEA_AGENTS_DEFAULT_WORKSPACE", "GSEA_AGENTS_AUTO_MERGE", "GSEA_AGENTS_AUTO_CONTINUE", "GSEA_AGENTS_SNAPSHOT_COPY_LIMIT"} {
		t.Setenv(name, "")
	}
	defaults, err := Load(LoadOptions{WorkingDirectory: project})
	if err != nil {
		t.Fatal(err)
	}
	if defaults.Agents.DefaultWorkspace != AgentsWorkspaceAuto || defaults.Agents.AutoMerge || !defaults.Agents.AutoContinue || defaults.Agents.SnapshotCopyLimit != 1<<30 {
		t.Fatalf("default workspace settings = %#v", defaults.Agents)
	}
	writeConfig(t, filepath.Join(home, configFileName), "[agents]\ndefault_workspace = \"Snapshot\"\nauto_merge = true\nauto_continue = false\nsnapshot_copy_limit = \"512MiB\"\n")
	writeConfig(t, filepath.Join(project, projectConfigDirectory, configFileName), "[agents]\nsnapshot_copy_limit = \"2GB\"\n")
	loaded, err := Load(LoadOptions{WorkingDirectory: project})
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Agents.DefaultWorkspace != AgentsWorkspaceSnapshot || !loaded.Agents.AutoMerge || loaded.Agents.AutoContinue || loaded.Agents.SnapshotCopyLimit != 2_000_000_000 {
		t.Fatalf("file workspace settings = %#v", loaded.Agents)
	}
	t.Setenv("GSEA_AGENTS_DEFAULT_WORKSPACE", "shared")
	t.Setenv("GSEA_AGENTS_AUTO_MERGE", "false")
	t.Setenv("GSEA_AGENTS_AUTO_CONTINUE", "true")
	t.Setenv("GSEA_AGENTS_SNAPSHOT_COPY_LIMIT", "4096")
	loaded, err = Load(LoadOptions{WorkingDirectory: project})
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Agents.DefaultWorkspace != AgentsWorkspaceShared || loaded.Agents.AutoMerge || !loaded.Agents.AutoContinue || loaded.Agents.SnapshotCopyLimit != 4096 {
		t.Fatalf("environment workspace settings = %#v", loaded.Agents)
	}
	for name, value := range map[string]string{
		"GSEA_AGENTS_DEFAULT_WORKSPACE":   "worktree",
		"GSEA_AGENTS_AUTO_MERGE":          "sometimes",
		"GSEA_AGENTS_AUTO_CONTINUE":       "often",
		"GSEA_AGENTS_SNAPSHOT_COPY_LIMIT": "1XB",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, value)
			if _, err := Load(LoadOptions{WorkingDirectory: project}); err == nil {
				t.Fatalf("%s=%s was accepted", name, value)
			}
		})
	}
}

func TestParseByteSize(t *testing.T) {
	for value, want := range map[string]int64{"1": 1, "10B": 10, "1KiB": 1024, "1 GiB": 1 << 30, "3mb": 3_000_000, "2TiB": 2 << 40} {
		if got, err := parseByteSize(value); err != nil || got != want {
			t.Fatalf("parseByteSize(%q) = %d, %v; want %d", value, got, err, want)
		}
	}
	for _, value := range []string{"", "0", "-1GiB", "GiB", "1.5GiB", "1PB", "9999999999TiB"} {
		if _, err := parseByteSize(value); err == nil {
			t.Fatalf("parseByteSize(%q) was accepted", value)
		}
	}
}

func TestUsageConfigLayersPricesAndBudget(t *testing.T) {
	t.Setenv("GSEA_TRUST_PROJECT_CONFIG", "true")
	home, project := t.TempDir(), t.TempDir()
	t.Setenv(HomeEnvironment, home)
	t.Setenv(ProfileEnvironment, "")
	defaults, err := Load(LoadOptions{WorkingDirectory: project})
	if err != nil || defaults.Usage.Prices != nil || defaults.Usage.Budget != (tokenusage.BudgetLimits{}) {
		t.Fatalf("default usage = %+v, err=%v", defaults.Usage, err)
	}
	writeConfig(t, filepath.Join(home, configFileName), `
[usage.prices."openai/gpt-large"]
input_micros_per_million = 2000000
output_micros_per_million = 8000000
[usage.prices.gpt-small]
input_micros_per_million = 100000
[usage.budget]
max_total_tokens = 5000
max_cost_micros = 900
`)
	writeConfig(t, filepath.Join(project, projectConfigDirectory, configFileName), `
[usage.prices."openai/gpt-large"]
cached_input_micros_per_million = 500000
[usage.budget]
max_cost_micros = 300
`)
	loaded, err := Load(LoadOptions{WorkingDirectory: project})
	if err != nil {
		t.Fatal(err)
	}
	wantPrices := map[string]tokenusage.Price{
		"openai/gpt-large": {InputMicrosPerMillion: 2_000_000, CachedInputMicrosPerMillion: 500_000, OutputMicrosPerMillion: 8_000_000},
		"gpt-small":        {InputMicrosPerMillion: 100_000},
	}
	if !maps.Equal(loaded.Usage.Prices, wantPrices) || loaded.Usage.Budget != (tokenusage.BudgetLimits{MaxTotalTokens: 5000, MaxCostMicros: 300}) {
		t.Fatalf("usage = %+v", loaded.Usage)
	}
	writeConfig(t, filepath.Join(project, projectConfigDirectory, configFileName), "[usage.budget]\nmax_input_tokens = -1\n")
	if _, err := Load(LoadOptions{WorkingDirectory: project}); err == nil || !strings.Contains(err.Error(), "usage.budget") {
		t.Fatalf("negative budget error = %v", err)
	}
	writeConfig(t, filepath.Join(project, projectConfigDirectory, configFileName), "[usage.prices.gpt-small]\noutput_micros_per_million = -1\n")
	if _, err := Load(LoadOptions{WorkingDirectory: project}); err == nil || !strings.Contains(err.Error(), "usage.prices") {
		t.Fatalf("negative price error = %v", err)
	}
}

func TestPostMergeCheckConfigLayersFilesAndEnvironment(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	t.Setenv(HomeEnvironment, home)
	t.Setenv(ProfileEnvironment, "")
	t.Setenv("GSEA_AGENTS_POST_MERGE_CHECK", "")
	t.Setenv("GSEA_AGENTS_POST_MERGE_CHECK_TIMEOUT", "")
	defaults, err := Load(LoadOptions{WorkingDirectory: project})
	if err != nil || defaults.Agents.PostMergeCheck != "" || defaults.Agents.PostMergeCheckTimeout != 10*time.Minute {
		t.Fatalf("defaults = %q %s, err=%v", defaults.Agents.PostMergeCheck, defaults.Agents.PostMergeCheckTimeout, err)
	}
	writeConfig(t, filepath.Join(home, configFileName), "[agents]\npost_merge_check = \" go test ./... \"\npost_merge_check_timeout = \"90s\"\n")
	loaded, err := Load(LoadOptions{WorkingDirectory: project})
	if err != nil || loaded.Agents.PostMergeCheck != "go test ./..." || loaded.Agents.PostMergeCheckTimeout != 90*time.Second {
		t.Fatalf("file = %q %s, err=%v", loaded.Agents.PostMergeCheck, loaded.Agents.PostMergeCheckTimeout, err)
	}
	t.Setenv("GSEA_AGENTS_POST_MERGE_CHECK", "make check")
	t.Setenv("GSEA_AGENTS_POST_MERGE_CHECK_TIMEOUT", "2m")
	loaded, err = Load(LoadOptions{WorkingDirectory: project})
	if err != nil || loaded.Agents.PostMergeCheck != "make check" || loaded.Agents.PostMergeCheckTimeout != 2*time.Minute {
		t.Fatalf("environment = %q %s, err=%v", loaded.Agents.PostMergeCheck, loaded.Agents.PostMergeCheckTimeout, err)
	}
	for _, value := range []string{"0s", "soon"} {
		t.Setenv("GSEA_AGENTS_POST_MERGE_CHECK_TIMEOUT", value)
		if _, err := Load(LoadOptions{WorkingDirectory: project}); err == nil {
			t.Fatalf("timeout %q accepted", value)
		}
	}
}

func writeConfig(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}
