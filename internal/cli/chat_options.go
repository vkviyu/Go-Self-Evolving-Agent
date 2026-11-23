package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/modelgateway"
	"github.com/vkviyu/go-self-evolving-agent/internal/sandbox"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

// chatOptions is the validated input to the chat application. Flag parsing
// stays at the CLI boundary so the turn loop can focus on conversation flow.
type chatOptions struct {
	dataDirectory           string
	dataExplicit            bool
	workspaceDirectory      string
	workspaceExplicit       bool
	sessionExplicit         bool
	requireExistingSession  bool
	resumeList              bool
	resumeAll               bool
	resumeLast              bool
	evaluationMode          bool
	model                   modelgateway.Config
	modelExplicit           bool
	providerExplicit        bool
	sessionID               session.ID
	resumeTurnID            string
	listRecoveries          bool
	abandonRequest          *session.AbandonRequest
	instruction             string
	maxIterations           int
	maxOutputTokens         int
	contextWindowTokens     int
	compactionTriggerTokens int
	maxRunningAgents        int
	headlessFormat          string
	promptFile              string
	shellSandbox            sandbox.Mode
	toolBridgeManifest      string
	toolBridgeSocket        string
}

const (
	headlessFormatStream = "stream"
	headlessFormatJSON   = "json"
)

func parseChatOptions(arguments []string, defaults config.Config, stderr io.Writer) (chatOptions, error) {
	return parseConversationOptions(arguments, defaults, stderr, false)
}

func parseConversationOptions(arguments []string, defaults config.Config, stderr io.Writer, resume bool) (chatOptions, error) {
	command := "chat"
	if resume {
		command = "resume"
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	profile := flags.String("profile", "", "configuration profile loaded from GSEA_HOME/<profile>.config.toml")
	flags.StringVar(profile, "p", "", "configuration profile loaded from GSEA_HOME/<profile>.config.toml")
	dataDirectory := flags.String("data-dir", defaults.DataDirectory, "local Agent data directory")
	workspaceDirectory := flags.String("workspace", defaults.WorkspaceDirectory, "workspace directory available to coding tools")
	evaluationMode := flags.Bool("evaluation-mode", false, "allow isolated headless evaluation to approve workspace write, edit and shell tools")
	provider := flags.String("provider", defaults.Model.Provider, "model provider: openai or ollama")
	wireAPI := flags.String("wire-api", defaults.Model.WireAPI, "OpenAI wire API: chat-completions or responses")
	modelName := flags.String("model", defaults.Model.Name, "provider model name (required)")
	tokenizerEncoding := flags.String("tokenizer-encoding", defaults.Model.TokenizerEncoding, "tokenizer encoding override, for example cl100k_base")
	baseURL := flags.String("base-url", defaults.Model.BaseURL, "provider base URL")
	apiKeyEnvironment := flags.String("api-key-env", defaults.Model.APIKeyEnvironment, "environment variable containing the provider API key")
	reasoningEffort := flags.String("reasoning-effort", defaults.Model.ReasoningEffort, "Responses reasoning effort: none, minimal, low, medium, high, or xhigh")
	requestTimeout := flags.Duration("timeout", defaults.Model.Timeout, "model timeout (full non-stream request, stream connect and read stalls)")
	sessionValue := flags.String("session", "", "session history to load; omitted creates a new session")
	resumeTurnID := flags.String("resume-turn", "", "explicitly resume one original turn; requires --session")
	listRecoveries := flags.Bool("list-recoveries", false, "list interrupted turns without executing them; requires --session")
	abandonTurn := flags.String("abandon-turn", "", "offline: abandon recovery of an interrupted root turn")
	abandonReason := flags.String("abandon-reason", "", "required reason for explicit abandonment")
	confirmAbandon := flags.Bool("confirm-abandon", false, "acknowledge prior effects were inspected and are not undone; recovery will be disabled")
	instruction := flags.String("system", defaults.Agent.Instruction, "Agent system instruction")
	maxIterations := flags.Int("max-iterations", defaults.Agent.MaxIterations, "maximum model/tool iterations per turn")
	maxOutputTokens := flags.Int("max-output-tokens", defaults.Agent.MaxOutputTokens, "maximum output tokens reserved per model request (0 = automatic)")
	contextWindowTokens := flags.Int("context-window-tokens", defaults.Agent.ContextWindowTokens, "model context capacity in tokens (0 = automatic)")
	compactionTriggerTokens := flags.Int("compaction-trigger-tokens", defaults.Agent.CompactionTriggerTokens, "context compaction threshold (0 = automatic)")
	maxRunningAgents := flags.Int("max-running-agents", defaults.Agents.MaxRunning, "maximum concurrently running child Agents; children that write the shared workspace still run one at a time")
	headlessFormat := flags.String("headless-format", headlessFormatStream, "non-interactive output format: stream or json")
	promptFile := flags.String("prompt-file", "", "run one non-interactive prompt from a file; use - for stdin")
	shellSandbox := flags.String("shell-sandbox", string(sandbox.ModeOS), "shell isolation boundary: os or container")
	toolBridgeManifest := flags.String("tool-bridge-manifest", "", "evaluation-only JSON manifest for invocation-scoped tools")
	toolBridgeSocket := flags.String("tool-bridge-socket", "", "evaluation-only Unix socket for invocation-scoped tools")
	var resumeList, resumeAll, resumeLast bool
	var leadingID string
	if resume {
		flags.BoolVar(&resumeList, "list", false, "list sessions without opening a conversation")
		flags.BoolVar(&resumeAll, "all", false, "include sessions from all workspaces and older sessions without a recorded workspace")
		flags.BoolVar(&resumeLast, "last", false, "open the most recent session in scope")
		flags.Usage = func() {
			fmt.Fprintln(stderr, "usage: gsea resume [SESSION_ID] [--list|--last] [--all] [chat options]")
			flags.PrintDefaults()
		}
		if len(arguments) > 0 && !strings.HasPrefix(arguments[0], "-") {
			leadingID, arguments = arguments[0], arguments[1:]
		}
	}
	if err := flags.Parse(arguments); err != nil {
		return chatOptions{}, err
	}
	if resume {
		if flags.NArg() > 1 || (leadingID != "" && flags.NArg() != 0) || *sessionValue != "" {
			return chatOptions{}, errors.New("resume accepts one positional Session ID")
		}
		if leadingID == "" && flags.NArg() == 1 {
			leadingID = flags.Arg(0)
		}
		if (resumeList && resumeLast) || (leadingID != "" && (resumeList || resumeLast)) || *evaluationMode {
			return chatOptions{}, errors.New("resume accepts one selection mode: a Session ID, --last, --list, or the picker")
		}
		if leadingID == "" && (*resumeTurnID != "" || *listRecoveries || *abandonTurn != "") {
			return chatOptions{}, errors.New("turn recovery with resume requires a positional Session ID")
		}
		*sessionValue = leadingID
	} else if flags.NArg() != 0 {
		return chatOptions{}, errors.New("chat does not accept positional arguments")
	}
	workspaceExplicit := false
	dataExplicit := false
	sessionExplicit := resume && leadingID != ""
	modelExplicit, providerExplicit := false, false
	flags.Visit(func(item *flag.Flag) {
		switch item.Name {
		case "workspace":
			workspaceExplicit = true
		case "data-dir":
			dataExplicit = true
		case "session":
			sessionExplicit = true
		case "model":
			modelExplicit = true
		case "provider":
			providerExplicit = true
		}
	})
	if *evaluationMode && (!workspaceExplicit || strings.TrimSpace(*workspaceDirectory) == "") {
		return chatOptions{}, errors.New("evaluation mode requires an explicit non-empty --workspace")
	}
	*shellSandbox = strings.ToLower(strings.TrimSpace(*shellSandbox))
	if sandbox.Mode(*shellSandbox) != sandbox.ModeOS && sandbox.Mode(*shellSandbox) != sandbox.ModeContainer {
		return chatOptions{}, errors.New("shell-sandbox must be os or container")
	}
	if sandbox.Mode(*shellSandbox) == sandbox.ModeContainer && !*evaluationMode {
		return chatOptions{}, errors.New("container shell sandbox requires evaluation mode")
	}
	*toolBridgeManifest = strings.TrimSpace(*toolBridgeManifest)
	*toolBridgeSocket = strings.TrimSpace(*toolBridgeSocket)
	if (*toolBridgeManifest == "") != (*toolBridgeSocket == "") {
		return chatOptions{}, errors.New("tool bridge manifest and socket must be provided together")
	}
	if *toolBridgeManifest != "" && !*evaluationMode {
		return chatOptions{}, errors.New("tool bridge requires evaluation mode")
	}
	if *requestTimeout <= 0 {
		return chatOptions{}, errors.New("timeout must be positive")
	}
	if *maxIterations <= 0 {
		return chatOptions{}, errors.New("max-iterations must be positive")
	}
	if *maxOutputTokens < 0 {
		return chatOptions{}, errors.New("max-output-tokens must be non-negative (0 = automatic)")
	}
	if *contextWindowTokens < 0 {
		return chatOptions{}, errors.New("context-window-tokens must be non-negative (0 = automatic)")
	}
	if *compactionTriggerTokens < 0 || (*contextWindowTokens > 0 && *compactionTriggerTokens >= *contextWindowTokens) {
		return chatOptions{}, errors.New("compaction-trigger-tokens must be non-negative (0 = automatic) and below an explicitly configured context window")
	}
	if *maxRunningAgents <= 0 {
		return chatOptions{}, errors.New("max-running-agents must be positive")
	}
	*headlessFormat = strings.ToLower(strings.TrimSpace(*headlessFormat))
	if *headlessFormat != headlessFormatStream && *headlessFormat != headlessFormatJSON {
		return chatOptions{}, errors.New("headless-format must be stream or json")
	}
	sessionID := session.ID(*sessionValue)
	*resumeTurnID = strings.TrimSpace(*resumeTurnID)
	*abandonTurn, *abandonReason = strings.TrimSpace(*abandonTurn), strings.TrimSpace(*abandonReason)
	var abandonment *session.AbandonRequest
	if *abandonTurn != "" {
		abandonment = &session.AbandonRequest{SessionID: sessionID, TurnID: *abandonTurn, Reason: *abandonReason, EffectsReviewed: *confirmAbandon}
		if err := abandonment.Validate(); err != nil {
			return chatOptions{}, err
		}
	} else if *abandonReason != "" || *confirmAbandon {
		return chatOptions{}, errors.New("abandon-reason and confirm-abandon require abandon-turn")
	}
	if *resumeTurnID != "" || *listRecoveries || abandonment != nil {
		if sessionID == "" {
			return chatOptions{}, errors.New("recovery requires an explicit --session")
		}
		if *resumeTurnID != "" && *listRecoveries {
			return chatOptions{}, errors.New("resume-turn and list-recoveries are mutually exclusive")
		}
		if abandonment != nil && (*resumeTurnID != "" || *listRecoveries) {
			return chatOptions{}, errors.New("abandonment cannot be combined with resume-turn or list-recoveries")
		}
		if *promptFile != "" || *evaluationMode {
			return chatOptions{}, errors.New("recovery cannot be combined with prompt-file or evaluation-mode")
		}
	}
	if sessionID == "" {
		var err error
		sessionID, err = newSessionID()
		if err != nil {
			return chatOptions{}, err
		}
	}
	if !sessionIDPattern.MatchString(string(sessionID)) {
		return chatOptions{}, errors.New("session ID must start with an alphanumeric character and contain only letters, digits, '.', '_' or '-'")
	}
	return chatOptions{
		dataDirectory:          *dataDirectory,
		dataExplicit:           dataExplicit,
		workspaceDirectory:     *workspaceDirectory,
		workspaceExplicit:      workspaceExplicit,
		sessionExplicit:        sessionExplicit,
		requireExistingSession: resume,
		resumeList:             resumeList,
		resumeAll:              resumeAll,
		resumeLast:             resumeLast,
		evaluationMode:         *evaluationMode,
		modelExplicit:          modelExplicit,
		providerExplicit:       providerExplicit,
		model: modelgateway.Config{
			Provider:          modelgateway.Provider(*provider),
			WireAPI:           modelgateway.WireAPI(*wireAPI),
			Model:             *modelName,
			TokenizerEncoding: strings.TrimSpace(*tokenizerEncoding),
			BaseURL:           *baseURL,
			APIKeyEnvironment: *apiKeyEnvironment,
			ReasoningEffort:   *reasoningEffort,
			Timeout:           *requestTimeout,
		},
		sessionID:               sessionID,
		resumeTurnID:            *resumeTurnID,
		listRecoveries:          *listRecoveries,
		abandonRequest:          abandonment,
		instruction:             *instruction,
		maxIterations:           *maxIterations,
		maxOutputTokens:         *maxOutputTokens,
		contextWindowTokens:     *contextWindowTokens,
		compactionTriggerTokens: *compactionTriggerTokens,
		maxRunningAgents:        *maxRunningAgents,
		headlessFormat:          *headlessFormat,
		promptFile:              strings.TrimSpace(*promptFile),
		shellSandbox:            sandbox.Mode(*shellSandbox),
		toolBridgeManifest:      *toolBridgeManifest,
		toolBridgeSocket:        *toolBridgeSocket,
	}, nil
}

func (options chatOptions) usesResponses() bool {
	return strings.EqualFold(string(options.model.Provider), string(modelgateway.ProviderOpenAI)) &&
		strings.EqualFold(string(options.model.WireAPI), string(modelgateway.WireAPIResponses))
}

// profileArgument extracts the profile before the complete flag set is parsed,
// because the selected profile supplies the defaults for that same flag set.
func profileArgument(arguments []string) (string, bool, error) {
	var profile string
	set := false
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		if argument == "--" {
			break
		}
		name := strings.TrimLeft(argument, "-")
		switch {
		case name == "profile" || name == "p":
			if index+1 >= len(arguments) {
				return "", false, errors.New("flag needs an argument: --profile")
			}
			index++
			profile = arguments[index]
			set = true
		case strings.HasPrefix(name, "profile=") || strings.HasPrefix(name, "p="):
			_, profile, _ = strings.Cut(name, "=")
			set = true
		}
	}
	if set && strings.TrimSpace(profile) == "" {
		return "", false, errors.New("config profile must not be empty")
	}
	return profile, set, nil
}
