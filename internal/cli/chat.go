package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentmanager"
	"github.com/vkviyu/go-self-evolving-agent/internal/app"
	"github.com/vkviyu/go-self-evolving-agent/internal/approval"
	"github.com/vkviyu/go-self-evolving-agent/internal/codingtools"
	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/modelgateway"
	"github.com/vkviyu/go-self-evolving-agent/internal/policy"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/sessionregistry"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
	"github.com/vkviyu/go-self-evolving-agent/internal/toolbridge"
	"github.com/vkviyu/go-self-evolving-agent/internal/tui"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacecoord"
)

var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func runChat(
	ctx context.Context,
	arguments []string,
	stdin io.Reader,
	stdout, stderr io.Writer,
	deps dependencies,
) (result error) {
	return runChatWithPrompt(ctx, arguments, stdin, stdout, stderr, deps, nil)
}

// runChatWithPrompt loads configuration, assembles chat resources and selects
// the interactive or headless entry point. The named result lets deferred
// application cleanup add its error to the error returned by the selected path.
func runChatWithPrompt(
	// ctx carries cancellation through startup, conversation execution and tools.
	ctx context.Context,
	// arguments contains the flags following the "chat" subcommand.
	arguments []string,
	// stdin supplies terminal input, piped chat input or a prompt when its path is "-".
	stdin io.Reader,
	// stdout receives chat results; stderr receives CLI diagnostics and status.
	stdout, stderr io.Writer,
	// deps supplies the model constructor, which tests can replace with a fake.
	deps dependencies,
	// nil means no prompt was supplied by the caller; a pointer selects one prompt.
	initialPrompt *string,
) (result error) {
	// Extract --profile/-p before loading the configuration that supplies flag defaults.
	profile, profileSet, err := profileArgument(arguments)
	// Stop if the profile flag is missing its value or explicitly selects an empty name.
	if err != nil {
		return err
	}

	// Empty load options let config.Load use the current directory and environment.
	loadOptions := config.LoadOptions{}
	// If the caller explicitly selected a profile, pass its name to config.Load.
	// Otherwise, leave Profile empty so config.Load can consult GSEA_PROFILE.
	if profileSet {
		loadOptions.Profile = profile
	}

	// Merge built-in defaults, user config, profile, project config and environment.
	resolved, err := config.Load(loadOptions)
	if err != nil {
		return err
	}

	// Use the loaded configuration as flag defaults, then apply explicit CLI flags.
	// options also records which flags were explicitly supplied for later merging.
	options, err := parseChatOptions(arguments, resolved, stderr)
	if err != nil {
		return err
	}

	// Keep the catalog handle for state updates and cleanup after a successful lookup.
	// It remains nil when this invocation does not open a Session catalog.
	var catalog *sessionregistry.Registry
	var sessionTitle string
	// Look up catalog metadata only when the user explicitly supplied --session.
	// A generated Session ID does not enter this branch.
	if options.sessionExplicit {
		// The central catalog is <GSEA_HOME>/control.db, independent of --data-dir.
		catalogPath := resolved.SessionRegistryPath()
		// Open an existing catalog. Checking first preserves the legacy behavior
		// where a missing catalog does not cause this chat invocation to create one.
		if _, statErr := os.Stat(catalogPath); statErr == nil {
			// Open SQLite and initialize the catalog table if necessary.
			catalog, err = sessionregistry.Open(ctx, catalogPath)
			if err != nil {
				return err
			}

			// Read the Session's resource paths and defaults, not its chat history.
			// Get accepts a string, so convert the typed Session ID explicitly.
			item, getErr := catalog.Get(ctx, string(options.sessionID))
			if getErr == nil {
				sessionTitle = strings.TrimSpace(item.Name)
				// If --data-dir was omitted, use the Session's registered data directory.
				if !options.dataExplicit {
					options.dataDirectory = item.DataDirectory
				}
				// If --workspace was omitted, use the Session's registered workspace.
				if !options.workspaceExplicit {
					options.workspaceDirectory = item.Workspace
				}
				// Use the registered model only if --model was omitted and the stored
				// model name contains a value. An explicit CLI model takes precedence.
				if !options.modelExplicit && strings.TrimSpace(item.Model) != "" {
					options.model.Model = item.Model
				}
				// Apply the same rule independently to --provider.
				// This conversion changes the Go type; it does not validate the provider.
				if !options.providerExplicit && strings.TrimSpace(item.Provider) != "" {
					options.model.Provider = modelgateway.Provider(item.Provider)
				}
			} else if errors.Is(getErr, sessionregistry.ErrNotFound) {
				// The catalog exists, but this Session is absent: fail startup.
				// Close here because the successful-path defer has not been registered.
				_ = catalog.Close()
				return fmt.Errorf("Session %q is not registered", options.sessionID)
			} else {
				// Preserve other lookup errors, such as SQLite or decoding failures.
				// Ignore a close error so the lookup error remains the reported cause.
				_ = catalog.Close()
				return getErr
			}
		} else if !errors.Is(statErr, os.ErrNotExist) {
			// A missing catalog is allowed; other inspection errors stop startup.
			// %w preserves the underlying error for errors.Is/errors.As.
			return fmt.Errorf("inspect Session catalog: %w", statErr)
		}
	}

	// Only a successfully opened and queried catalog participates in state updates.
	if catalog != nil {
		// Defers run in reverse order: the stopped-state update below runs before Close.
		defer catalog.Close()
		// Record the current process as running. State-update errors are ignored here.
		_ = catalog.UpdateState(ctx, string(options.sessionID), sessionregistry.StateRunning, os.Getpid())
		// On every normal function return, mark the Session stopped and clear its PID.
		// A fresh context allows this write even if the command context was canceled.
		// This records "stopped" on error returns too; it does not select "failed".
		defer func() {
			_ = catalog.UpdateState(context.Background(), string(options.sessionID), sessionregistry.StateStopped, 0)
		}()
	}

	// Reject two competing prompt sources: a caller-provided prompt and a prompt file.
	if initialPrompt != nil && options.promptFile != "" {
		return errors.New("prompt-file cannot be combined with a programmatic prompt")
	}
	// A supplied prompt starts new work; recovery operations address existing work.
	if initialPrompt != nil && (options.resumeTurnID != "" || options.listRecoveries || options.abandonRequest != nil) {
		return errors.New("recovery cannot be combined with a new prompt")
	}

	// If --prompt-file is set, read its contents and use them as the single prompt.
	if options.promptFile != "" {
		// Hold bytes from either stdin or the named file until reading succeeds.
		var prompt []byte
		if options.promptFile == "-" {
			// "-" means read stdin. Read at most 4 MiB plus one byte so an oversized
			// input can be detected without consuming an unbounded stream.
			prompt, err = io.ReadAll(io.LimitReader(stdin, 4<<20+1))
		} else {
			// Read the named file; the size check below runs after this read completes.
			prompt, err = os.ReadFile(options.promptFile)
		}
		if err != nil {
			return fmt.Errorf("read prompt file: %w", err)
		}
		// 4<<20 is 4 MiB. Reject oversized input from either source.
		if len(prompt) > 4<<20 {
			return errors.New("prompt file exceeds 4 MiB")
		}
		// Convert the bytes to text and retain a pointer, including for an empty file.
		value := string(prompt)
		initialPrompt = &value
	}

	// Select TUI mode only when there is no supplied prompt, recovery listing or
	// abandonment request, and both streams pass isTerminal.
	// --resume-turn is allowed here because the TUI can resume an existing turn.
	interactive := initialPrompt == nil && !options.listRecoveries && options.abandonRequest == nil && isTerminal(stdin) && isTerminal(stdout)
	// TUI mode cannot use the JSON headless result format.
	if interactive && options.headlessFormat != headlessFormatStream {
		return errors.New("headless-format is only available with non-interactive stdin and stdout")
	}
	// Evaluation mode must run without the interactive TUI.
	if interactive && options.evaluationMode {
		return errors.New("evaluation mode requires non-interactive stdin and stdout")
	}
	// For evaluation, reject workspaces that are filesystem roots or contain
	// the user's home directory, according to validateEvaluationWorkspace.
	if options.evaluationMode {
		if err := validateEvaluationWorkspace(options.workspaceDirectory); err != nil {
			return err
		}
	}

	// Copy the selected data directory into the configuration used to open the app.
	resolved.DataDirectory = options.dataDirectory
	// Copy the selected workspace, including CLI or Session catalog overrides.
	resolved.WorkspaceDirectory = options.workspaceDirectory
	// Use the Goal settings parsed for this invocation.
	resolved.Goal = options.goalMode
	// Keep the application model name aligned with the selected chat model.
	// The model client receives the full options.model configuration below.
	resolved.Model.Name = options.model.Model
	// Apply the selected system instruction and per-turn iteration limit.
	resolved.Agent.Instruction = options.instruction
	resolved.Agent.MaxIterations = options.maxIterations

	// Handle abandonment before creating the normal app, model or Supervisor.
	if options.abandonRequest != nil {
		// Persist the abandonment decision and obtain a receipt of the operation.
		receipt, err := app.AbandonRecovery(ctx, resolved, *options.abandonRequest)
		if err != nil {
			return err
		}
		// Explain that abandonment disables recovery without undoing prior effects.
		fmt.Fprintln(stderr, "Recovery abandoned. Prior side effects, events, checkpoints and plugin artifacts were NOT undone or deleted.")
		// Write the receipt and finish this command without starting a conversation.
		return writeAbandonment(stdout, receipt)
	}

	// Leave extra tool bindings nil unless this invocation configured a tool bridge.
	var bridgeBindings []tool.Binding
	if options.toolBridgeManifest != "" {
		// Load invocation-scoped tool descriptors and socket-backed executors.
		// parseChatOptions already requires evaluation mode and both bridge paths.
		bridgeBindings, err = toolbridge.Load(options.toolBridgeManifest, options.toolBridgeSocket)
		if err != nil {
			return err
		}
	}

	// Open local runtime resources, including the database, workspace tools,
	// approval broker and plugin services. The app owns their lifecycle.
	application, err := app.OpenWithOptions(ctx, resolved, app.OpenOptions{
		// Select the OS sandbox or an explicitly selected existing container boundary.
		ShellSandbox: options.shellSandbox,
		// Register any additional tools supplied by the evaluation bridge.
		ToolBindings: bridgeBindings,
	})
	if err != nil {
		return err
	}

	// Close the app on every subsequent return and retain any cleanup error.
	// A return expression first assigns result; this defer then joins Close's error.
	// This runs before the earlier catalog state-update and close defers.
	defer func() { result = errors.Join(result, application.Close()) }()

	// Listing recoverable work needs local runtime resources but no model client.
	if options.listRecoveries {
		items, err := application.Recoveries(ctx, options.sessionID)
		if err != nil {
			return err
		}
		// Write the list and return before creating the model or Supervisor.
		return writeRecoveries(stdout, items)
	}

	// The remaining paths execute a conversation and require a model constructor.
	if deps.newModel == nil {
		return errors.New("model builder is unavailable")
	}
	// Build the model client through the injected constructor.
	// Production uses modelgateway.New; tests can supply a fake model.
	chatModel, err := deps.newModel(ctx, options.model)
	if err != nil {
		return err
	}

	// Ensure the Session has a root Agent thread and obtain its Agent/Team identity.
	// Ignore the helper's second return value because this path only needs root.
	root, _, err := application.EnsureRootThread(ctx, options.sessionID)
	if err != nil {
		return fmt.Errorf("ensure root Agent thread: %w", err)
	}

	// Collect bridge tool names for the evaluation approval allowlist.
	// Start with zero elements and reserve capacity for all bridge bindings.
	bridgeToolNames := make([]string, 0, len(bridgeBindings))
	for _, binding := range bridgeBindings {
		bridgeToolNames = append(bridgeToolNames, binding.Descriptor.Name)
	}

	// Use the approval broker for TUI requests, deny approval in ordinary headless
	// mode, or use the evaluation allowlist. ... expands tool names into arguments.
	approver, err := selectChatApprover(interactive, options.evaluationMode, application.Approval(), bridgeToolNames...)
	if err != nil {
		return err
	}

	// Combine the model, runtime settings, root actor identity and approver.
	// rootRuntimeConfig also supplies token settings, tokenizer and usage ledger.
	// The Factory prepares context and constructs individual Agent turns.
	factory, err := application.NewAgentFactory(chatModel, rootRuntimeConfig(options, root.ActorScope(), application.Orchestration()), approver)
	if err != nil {
		return err
	}

	// Ask the same approver before a child's snapshot changes the workspace,
	// unless [agents] auto_merge applies conflict-free plans directly.
	mergeExecutor, err := application.NewMergeExecutor(nil, approver)
	if err != nil {
		return err
	}

	// Create the Supervisor that manages delegated Agent work.
	supervisor, err := application.NewSupervisor(agentmanager.Config{
		// Prepare initial context for a child Agent using the shared Factory.
		ContextSeeder: newChildContextSeeder(factory),
		// Run or resume child conversations through the Factory-backed adapter.
		Runner: newChildRunner(factory),
		// Deliver snapshot work through the approving merge executor.
		MergeExecutor: mergeExecutor,
		// Apply the [agents] limits; the command-line value overrides max_running.
		MaxRunningAgents:  options.maxRunningAgents,
		MaxLiveAgents:     resolved.Agents.MaxLive,
		MaxDepth:          resolved.Agents.MaxDepth,
		MaxChildren:       resolved.Agents.MaxChildren,
		ModelBucketLimits: resolved.Agents.ModelLimits,
		// Isolate writable children as [agents] default_workspace selects.
		DefaultWorkspace: agentmanager.WorkspaceDefault(resolved.Agents.DefaultWorkspace),
	})
	if err != nil {
		return err
	}

	// Bind the root mailbox to the Factory, acknowledge messages of turns that
	// committed before a restart and load the Session's durable history.
	// Child results then reach the root inside its turns or as a wake turn.
	rootChat, err := openRootConversation(ctx, factory, supervisor, root)
	if err != nil {
		return err
	}

	// If interactive, run the terminal UI until it exits or returns an error.
	if interactive {
		if sessionTitle == "" {
			// Read the durable first query, rather than compacted model history,
			// so a resumed conversation keeps its original title.
			sessionTitle, err = factory.FirstUserPrompt(ctx, options.sessionID)
			if err != nil {
				return err
			}
		}
		return tui.Run(ctx, stdin, stdout, tui.Config{
			// Route user turns and recovery operations to the root Conversation.
			Conversation: rootChat,
			SessionTitle: sessionTitle,
			// Supply model and provider labels for the UI.
			Model:    options.model.Model,
			Provider: string(options.model.Provider),
			// Display the workspace resolved by the application.
			Workspace: application.WorkspaceDirectory(),
			// Supply the selected reasoning setting for display.
			ReasoningEffort: options.model.ReasoningEffort,
			// Let the UI receive and answer tool approval requests.
			Approval: application.Approval(),
			// Scope the Agent activity display to this team.
			TeamID:   root.TeamID,
			Activity: supervisor,
			// Wake the idle root Agent when child results arrive.
			AutoContinue: resolved.Agents.AutoContinue,
			// Let the /agents panel interrupt children, review merges and
			// open child transcripts.
			Agents:      supervisor,
			Transcripts: factory,
			// If set, let the UI resume this turn instead of starting a new one.
			ResumeTurnID: options.resumeTurnID,
		})
	}

	// Without the TUI, an explicit turn ID selects checkpoint-based recovery.
	if options.resumeTurnID != "" {
		// Keep status information on stderr, separate from the result on stdout.
		printHeadlessHeader(stderr, rootChat, options)
		return runHeadlessResume(ctx, stdout, rootChat, options.resumeTurnID, options.headlessFormat)
	}

	// A supplied prompt selects one headless request; dereference its text here.
	if initialPrompt != nil {
		return runHeadlessPrompt(ctx, stdout, stderr, rootChat, options, *initialPrompt)
	}

	// Otherwise, read chat input from stdin using the headless conversation loop.
	// Returning from any execution path above also runs the registered defers.
	return runHeadlessChat(ctx, stdin, stdout, stderr, rootChat, options)
}

func selectChatApprover(interactive, evaluationMode bool, broker *approval.Broker, invocationTools ...string) (policy.Approver, error) {
	if interactive {
		if evaluationMode {
			return nil, errors.New("evaluation mode requires non-interactive stdin and stdout")
		}
		return broker, nil
	}
	if !evaluationMode {
		// Piped/headless runs have no safe place to ask for approval. They fail
		// closed unless the caller explicitly selects isolated evaluation mode.
		return approval.DenyApprover{}, nil
	}
	allowed := []string{}
	if len(invocationTools) == 0 {
		allowed = append(allowed,
			codingtools.WriteToolName,
			codingtools.EditToolName,
			codingtools.ShellToolName,
			// Snapshot merges deliver the same writes a shared child makes.
			workspacecoord.MergeApplyToolName,
		)
	} else {
		// A harness-owned environment gets an exclusive mutation allowlist.
		// In particular, its prompt cannot use shell_exec to inspect the GSEA
		// process environment that contains the model credential.
		allowed = append(allowed, invocationTools...)
	}
	return approval.NewToolAllowlistApprover(allowed...), nil
}

func validateEvaluationWorkspace(workspace string) error {
	absolute, err := filepath.Abs(strings.TrimSpace(workspace))
	if err != nil {
		return fmt.Errorf("resolve evaluation workspace: %w", err)
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return fmt.Errorf("resolve evaluation workspace symlinks: %w", err)
	}
	if filepath.Dir(canonical) == canonical {
		return errors.New("evaluation workspace must not be a filesystem root")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		return nil
	}
	relativeHome, err := filepath.Rel(canonical, home)
	if err == nil && filepath.IsLocal(relativeHome) {
		return errors.New("evaluation workspace must not contain the user home directory")
	}
	return nil
}

func newSessionID() (session.ID, error) {
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("generate session ID: %w", err)
	}
	return session.ID("chat-" + time.Now().UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(random[:])), nil
}

func isTerminal(value any) bool {
	file, ok := value.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
