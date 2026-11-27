package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentmanager"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/app"
	"github.com/vkviyu/go-self-evolving-agent/internal/ateprocess"
	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
	"github.com/vkviyu/go-self-evolving-agent/internal/tui"
)

type pendingConversation struct {
	id string
}

func (conversation pendingConversation) ID() session.ID {
	return session.ID(conversation.id)
}

func (pendingConversation) History() []*schema.Message {
	return nil
}

func (pendingConversation) CompletedTurns() int {
	return 0
}

func (pendingConversation) RunTurn(context.Context, string, agentruntime.EventSink) (agentruntime.Result, error) {
	return agentruntime.Result{}, errors.New("chat startup is still in progress")
}

type interactiveSetupResult struct {
	cleanup func() error
	err     error
}

// startupPublisher serializes progress with the terminal attachment. Storage
// observers must return promptly, including when the terminal has exited.
type startupPublisher struct {
	mu     sync.Mutex
	closed bool
	states chan tui.StartupState
}

// phase publishes a startup phase to the TUI while the startup channel is open.
func (publisher *startupPublisher) phase(ctx context.Context, phase tui.StartupPhase) {
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	if publisher.closed {
		return
	}
	select {
	case publisher.states <- tui.StartupState{Phase: phase, At: time.Now()}:
	case <-ctx.Done():
	}
}

func (publisher *startupPublisher) observe(_ context.Context, progress ateprocess.Progress) {
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	if publisher.closed {
		return
	}
	select {
	case publisher.states <- tui.StartupState{At: progress.At, Storage: &progress}:
	default:
		// Observation cannot delay local storage. Phase and final state
		// delivery remain reliable even if verbose storage updates overflow.
	}
}

func (publisher *startupPublisher) finish(ctx context.Context, state tui.StartupState) {
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	if publisher.closed {
		return
	}
	state.At = time.Now()
	select {
	case publisher.states <- state:
	case <-ctx.Done():
	}
	publisher.closed = true
	close(publisher.states)
}

func runInteractiveChat(
	ctx context.Context,
	application *app.App,
	resolved config.Config,
	options chatOptions,
	selected chatSession,
	stdin io.Reader,
	stdout io.Writer,
	deps dependencies,
	initialPrompt *string,
	bridgeBindings []tool.Binding,
) error {
	setupContext, cancel := context.WithCancel(ctx)
	defer cancel()
	sessionTitle := applyChatSession(&options, selected)

	ready := make(chan struct{})
	startupState := make(chan tui.StartupState, 256)
	publisher := &startupPublisher{states: startupState}
	var setupErr error
	conversation := &startupConversation{
		base:     pendingConversation{id: string(options.sessionID)},
		ready:    ready,
		readyErr: func() error { return setupErr },
	}
	setupResults := make(chan interactiveSetupResult, 1)

	go func() {
		result := interactiveSetupResult{}
		attached := false
		defer func() {
			if !attached {
				setupErr = result.err
				close(ready)
				publisher.finish(setupContext, tui.StartupState{Err: result.err})
			}
			setupResults <- result
		}()

		publisher.phase(setupContext, tui.StartupModelInitialization)
		chatModel, err := deps.newModel(setupContext, options.model)
		if err != nil {
			result.err = fmt.Errorf("initialize model: %w", err)
			return
		}

		bridgeToolNames := make([]string, 0, len(bridgeBindings))
		for _, binding := range bridgeBindings {
			bridgeToolNames = append(bridgeToolNames, binding.Descriptor.Name)
		}
		approver, err := selectChatApprover(true, false, application.Approval(), bridgeToolNames...)
		if err != nil {
			result.err = err
			return
		}

		var root orchestration.RootThread
		var factory *agentruntime.Factory
		var supervisor *agentmanager.Supervisor
		var rootChat tui.Conversation
		var rootTranscript []*schema.Message
		startupContext := ateprocess.WithObserver(setupContext, publisher.observe)
		publisher.phase(setupContext, tui.StartupRootOwner)
		err = application.WithStorageBatch(startupContext, func(batchContext context.Context) error {
			var err error
			root, _, err = application.EnsureRootThreadWithOptions(
				batchContext, options.sessionID, app.EnsureRootThreadOptions{DeferMaintenance: true},
			)
			if err != nil {
				return fmt.Errorf("ensure root Agent thread: %w", err)
			}
			publisher.phase(setupContext, tui.StartupRuntimeFactory)
			factory, err = application.NewAgentFactory(
				chatModel, rootRuntimeConfig(options, root.ActorScope(), application.Orchestration()), approver,
			)
			if err != nil {
				return fmt.Errorf("initialize runtime factory: %w", err)
			}
			mergeExecutor, err := application.NewMergeExecutor(nil, approver)
			if err != nil {
				return err
			}
			publisher.phase(setupContext, tui.StartupSupervisor)
			// Recovery I/O uses the batch, but child lifetimes must never
			// inherit its scoped context or native process.
			supervisor, err = application.NewSupervisorContext(batchContext, agentmanager.Config{
				ContextSeeder:         newChildContextSeeder(factory),
				Runner:                newChildRunner(factory),
				MergeExecutor:         mergeExecutor,
				MaxRunningAgents:      options.maxRunningAgents,
				MaxLiveAgents:         resolved.Agents.MaxLive,
				MaxDepth:              resolved.Agents.MaxDepth,
				MaxChildren:           resolved.Agents.MaxChildren,
				ModelBucketLimits:     resolved.Agents.ModelLimits,
				DefaultWorkspace:      agentmanager.WorkspaceDefault(resolved.Agents.DefaultWorkspace),
				DeferRecoveryDispatch: true,
				RunContext:            setupContext,
			})
			if err != nil {
				return fmt.Errorf("initialize supervisor: %w", err)
			}
			publisher.phase(setupContext, tui.StartupSessionMailbox)
			rootChat, err = openRootConversation(batchContext, factory, supervisor, root)
			if err != nil {
				return fmt.Errorf("open session / mailbox: %w", err)
			}
			publisher.phase(setupContext, tui.StartupMaintenance)
			if err := application.ResumeSessionMaintenance(batchContext, root.SessionID); err != nil {
				return fmt.Errorf("resume session maintenance: %w", err)
			}
			publisher.phase(setupContext, tui.StartupTranscript)
			rootTranscript, err = factory.LoadTranscript(batchContext, root.SessionID)
			if err != nil {
				return fmt.Errorf("load chat transcript: %w", err)
			}
			return nil
		})
		if err != nil {
			result.err = err
			return
		}

		// Mandatory maintenance and native process shutdown both precede
		// dispatch, readiness and optional asynchronous catalog registration.
		maintenanceDone := make(chan struct{})
		close(maintenanceDone)
		supervisor.StartAvailableAfter(maintenanceDone)
		conversation.setBase(rootChat)
		close(ready)
		attached = true
		publisher.finish(setupContext, tui.StartupState{
			Ready:          true,
			TeamID:         root.TeamID,
			Activity:       supervisor,
			Agents:         supervisor,
			Transcripts:    factory,
			RootTranscript: rootTranscript,
		})
		stopCatalog, err := trackChatSessionAsync(
			setupContext,
			application,
			resolved,
			options,
			selected,
			application.WorkspaceDirectory(),
		)
		if err != nil {
			result.err = fmt.Errorf("register chat session: %w", err)
			return
		}
		result.cleanup = stopCatalog
	}()

	memoryBrowser, _ := application.MemoryBrowser()
	uiErr := tui.Run(ctx, stdin, stdout, tui.Config{
		Conversation:    conversation,
		SessionTitle:    sessionTitle,
		Model:           options.model.Model,
		Provider:        string(options.model.Provider),
		Workspace:       application.WorkspaceDirectory(),
		ReasoningEffort: options.model.ReasoningEffort,
		Approval:        application.Approval(),
		AutoContinue:    resolved.Agents.AutoContinue,
		ResumeTurnID:    options.resumeTurnID,
		Startup:         startupState,
		Memory:          memoryBrowser,
	})
	cancel()
	result := <-setupResults
	var cleanupErr error
	if result.cleanup != nil {
		cleanupErr = result.cleanup()
	}
	if errors.Is(result.err, context.Canceled) && ctx.Err() == nil {
		// The terminal may close while ATE startup is still unwinding. The
		// cancellation is owned by this command and is not a startup failure.
		result.err = nil
	}
	if errors.Is(cleanupErr, context.Canceled) && ctx.Err() == nil {
		cleanupErr = nil
	}
	return errors.Join(uiErr, result.err, cleanupErr)
}
