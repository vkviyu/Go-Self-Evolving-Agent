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
	"github.com/vkviyu/go-self-evolving-agent/internal/policy"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
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
) error {
	return runChatWithPrompt(ctx, arguments, stdin, stdout, stderr, deps, nil)
}

// runChatWithPrompt loads configuration and delegates the selected chat mode.
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
) error {
	resolved, err := loadChatConfig(arguments)
	if err != nil {
		return err
	}

	// Use the loaded configuration as flag defaults, then apply explicit CLI flags.
	// options also records which flags were explicitly supplied for later merging.
	options, err := parseChatOptions(arguments, resolved, stderr)
	if err != nil {
		return err
	}

	return runConfiguredChat(ctx, resolved, options, stdin, stdout, stderr, deps, initialPrompt, nil)
}

func loadChatConfig(arguments []string) (config.Config, error) {
	profile, _, err := profileArgument(arguments)
	if err != nil {
		return config.Config{}, err
	}
	return config.Load(config.LoadOptions{Profile: profile})
}

func runConfiguredChat(
	ctx context.Context,
	resolved config.Config,
	options chatOptions,
	stdin io.Reader,
	stdout, stderr io.Writer,
	deps dependencies,
	initialPrompt *string,
	resumeCatalog *app.SessionCatalog,
) (result error) {
	var err error
	var selected chatSession
	var sessionTitle string
	if options.sessionExplicit && !options.evaluationMode {
		if resumeCatalog != nil {
			selected, err = findChatSessionFromCatalog(ctx, resumeCatalog, resolved, options)
		} else {
			selected, err = findChatSession(ctx, resolved, options)
		}
		if err != nil {
			return err
		}
		if options.requireExistingSession && selected.ID == "" {
			return fmt.Errorf("session %q was not found; use resume --list --all to browse saved sessions", options.sessionID)
		}
		sessionTitle = applyChatSession(&options, selected)
		if selected.ID != "" && selected.Workspace == "" && !options.workspaceExplicit {
			workspace, err := workspaceForChat(options.workspaceDirectory)
			if err != nil {
				return err
			}
			fmt.Fprintf(stderr, "Session %s has no recorded workspace; using %s\n", options.sessionID, workspace)
		}
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
		fmt.Fprintln(stderr, "Recovery abandoned. Prior side effects, events and checkpoints were NOT undone or deleted.")
		// Write the receipt and finish this command without starting a conversation.
		return writeAbandonment(stdout, receipt)
	}

	if !options.listRecoveries {
		data := resolved.DataDirectory
		if data == "" {
			data = resolved.HomeDirectory
		}
		var finish func(error)
		ctx, finish = observeChat(ctx, data, options.sessionID, interactive, stderr)
		// Registered before opening the app so workers finish before the trace closes.
		defer func() { finish(result) }()
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

	// The app owns the database, workspace tools and approval broker.
	application, err := app.OpenWithOptions(ctx, resolved, app.OpenOptions{
		// Select the OS sandbox or an explicitly selected existing container boundary.
		ShellSandbox: options.shellSandbox,
		// Register any additional tools supplied by the evaluation bridge.
		ToolBindings:   bridgeBindings,
		SessionCatalog: resumeCatalog,
	})
	if err != nil {
		return err
	}

	// Update the catalog while the runtime still holds the data directory lock.
	var stopCatalog func() error
	defer func() {
		if stopCatalog != nil {
			if err := stopCatalog(); err != nil {
				result = errors.Join(result, fmt.Errorf("stop chat catalog: %w", err))
			}
		}
		if err := application.Close(); err != nil {
			result = errors.Join(result, fmt.Errorf("close application: %w", err))
		}
	}()

	// Listing recoverable work needs local runtime resources but no model client.
	if options.listRecoveries {
		items, err := application.Recoveries(ctx, options.sessionID)
		if err != nil {
			return err
		}
		// Write the list and return before creating the model or Supervisor.
		return writeRecoveries(stdout, items)
	}

	if interactive {
		return runInteractiveChat(
			ctx,
			application,
			resolved,
			options,
			selected,
			stdin,
			stdout,
			deps,
			initialPrompt,
			bridgeBindings,
		)
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
	root, _, err := application.EnsureRootThreadWithOptions(
		ctx,
		options.sessionID,
		app.EnsureRootThreadOptions{DeferMaintenance: interactive},
	)
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
		// Workers and recovery keep the invocation's observer across dispatches.
		// The Supervisor retains its own cancellation and shutdown boundary.
		RunContext: context.WithoutCancel(ctx),
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
		// Interactive startup presents the root conversation first; resume
		// queued children after the TUI is ready.
		// Chat startup always resumes queued children after the root path is
		// ready; headless callers must not block on the same global scan.
		DeferRecoveryDispatch: true,
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
	var maintenanceDone chan struct{}
	var maintenanceErr error
	if interactive {
		maintenanceDone = make(chan struct{})
		go func() {
			maintenanceErr = application.ResumeSessionMaintenance(ctx, root.SessionID)
			close(maintenanceDone)
		}()
		supervisor.StartAvailableAfter(maintenanceDone)
	} else {
		supervisor.StartAvailableAsync()
	}

	if !options.evaluationMode {
		if interactive {
			stopCatalog, err = trackChatSessionAsync(ctx, application, resolved, options, selected, application.WorkspaceDirectory())
		} else {
			stopCatalog, err = trackChatSession(ctx, application, resolved, options, selected, application.WorkspaceDirectory())
		}
		if err != nil {
			return fmt.Errorf("register chat session: %w", err)
		}
	}

	// If interactive, run the terminal UI until it exits or returns an error.
	if interactive {
		displayConversation := tui.Conversation(rootChat)
		if maintenanceDone != nil {
			displayConversation = &startupConversation{
				base:     rootChat,
				ready:    maintenanceDone,
				readyErr: func() error { return maintenanceErr },
			}
		}
		if sessionTitle == "" {
			// Read the durable first query, rather than compacted model history,
			// so a resumed conversation keeps its original title.
			sessionTitle, err = factory.FirstUserPrompt(ctx, options.sessionID)
			if err != nil {
				return err
			}
		}
		memoryBrowser, _ := application.MemoryBrowser()
		return tui.Run(ctx, stdin, stdout, tui.Config{
			// Route user turns and recovery operations to the root Conversation.
			Conversation: displayConversation,
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
			Memory:      memoryBrowser,
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
