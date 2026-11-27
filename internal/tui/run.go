// Package tui implements the interactive terminal presentation layer. It owns
// terminal state and never constructs models, stores or Agent runtimes.
package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/cloudwego/eino/schema"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentmanager"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/approval"
	"github.com/vkviyu/go-self-evolving-agent/internal/ateprocess"
	"github.com/vkviyu/go-self-evolving-agent/internal/experience"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacecoord"
)

// Conversation is the application boundary required by the terminal UI.
type Conversation interface {
	// ID identifies the session currently displayed in the terminal.
	ID() session.ID
	// History returns the conversation messages used to initialize the displayed
	// transcript.
	History() []*schema.Message
	// CompletedTurns reports how many conversation turns have finished successfully.
	CompletedTurns() int
	// RunTurn submits the user's input and sends incremental runtime events to the
	// supplied sink.
	RunTurn(context.Context, string, agentruntime.EventSink) (agentruntime.Result, error)
}

// ContextConversation is implemented by the durable conversation service. It
// is optional at the UI boundary so lightweight embedders and tests can keep a
// minimal Conversation implementation.
type ContextConversation interface {
	// ContextUsage returns the current estimate of the conversation's model input and its
	// limits.
	ContextUsage() agentruntime.ContextUsage
	// Compress explicitly compacts the conversation and returns whether it changed and the
	// resulting usage.
	Compress(context.Context) (bool, agentruntime.ContextUsage, error)
}

type ContextEventConversation interface {
	// CompressWithSink reports progress during explicit compaction and returns the
	// resulting context usage.
	CompressWithSink(context.Context, agentruntime.EventSink) (bool, agentruntime.ContextUsage, error)
}

type pendingContextConversation interface {
	// ContextUsageFor estimates the context usage that would result from submitting the
	// pending input.
	ContextUsageFor(string) agentruntime.ContextUsage
}

// BackgroundConversation is implemented by a root conversation that can wake
// itself for Agent messages that arrived while it was idle.
type BackgroundConversation interface {
	// PendingBackground reports how many background messages are waiting for a wake turn.
	PendingBackground(context.Context) (int, error)
	// RunBackgroundTurn processes waiting agent messages and publishes progress to the
	// event sink.
	RunBackgroundTurn(context.Context, agentruntime.EventSink) (agentruntime.Result, error)
}

// AgentControl is the narrow mutation boundary used by the /agents panel.
type AgentControl interface {
	Interrupt(context.Context, orchestration.AgentID) error
	ReviewMerges(orchestration.TeamID) error
	DiscardMerge(context.Context, orchestration.MergeID, string) (workspacecoord.MergeRequest, error)
}

// TranscriptReader loads a display transcript for one Agent session.
type TranscriptReader interface {
	LoadTranscript(context.Context, session.ID) ([]*schema.Message, error)
}

// StartupPhase describes local runtime assembly, before any user turn runs.
type StartupPhase uint8

const (
	StartupModelInitialization StartupPhase = iota + 1
	StartupRootOwner
	StartupRuntimeFactory
	StartupSupervisor
	StartupSessionMailbox
	StartupMaintenance
	StartupTranscript
)

// StartupState carries ordered setup progress and one terminal attachment.
// Ready or Err terminates startup; producers must then close the channel.
// Storage contains metadata only, never native protocol text.
type StartupState struct {
	Phase       StartupPhase
	At          time.Time
	Storage     *ateprocess.Progress
	Ready       bool
	Err         error
	TeamID      orchestration.TeamID
	Activity    agentmanager.TeamSnapshotReader
	Agents      AgentControl
	Transcripts TranscriptReader
	// RootTranscript is the durable display history loaded before attachment.
	RootTranscript []*schema.Message
}

// Config contains display metadata and the conversation application service.
type Config struct {
	Conversation Conversation
	Model        string
	Provider     string
	Workspace    string
	// SessionTitle is an optional user-assigned title. Otherwise the first
	// real user question supplies a stable local title, including on resume.
	SessionTitle    string
	ReasoningEffort string
	Approval        *approval.Broker
	TeamID          orchestration.TeamID
	Activity        agentmanager.TeamSnapshotReader
	// AutoContinue wakes an idle root Agent when Agent messages arrive. It
	// requires a Conversation that implements BackgroundConversation.
	AutoContinue bool
	// Agents interrupts children and reviews merges from the /agents panel.
	Agents AgentControl
	// Transcripts opens a child Agent's transcript from the /agents panel.
	Transcripts  TranscriptReader
	ResumeTurnID string
	Memory       experience.MemoryBrowser
	Startup      <-chan StartupState
}

// Run takes exclusive control of the terminal until the user exits.
func Run(ctx context.Context, input io.Reader, output io.Writer, config Config) error {
	if config.Conversation == nil {
		return errors.New("TUI requires a conversation")
	}
	if input == nil || output == nil {
		return errors.New("TUI requires terminal input and output")
	}
	model, err := NewModel(ctx, config)
	if err != nil {
		return err
	}
	// A previous program may have left the terminal inside a hyperlink.
	if _, err := io.WriteString(output, ansi.ResetHyperlink()); err != nil {
		return fmt.Errorf("reset terminal hyperlink state: %w", err)
	}
	program := tea.NewProgram(
		model,
		tea.WithContext(ctx),
		tea.WithInput(input),
		tea.WithOutput(wrapCursorWriter(output)),
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
		tea.WithoutSignalHandler(),
	)
	if _, err := program.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("run terminal UI: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}
