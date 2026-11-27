package tui

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/conversation"
)

// RecoveryConversation is optional; display code never reads the event store
// or decides whether a checkpoint is safe to execute.
type RecoveryConversation interface {
	// Recoveries returns interrupted turns available for inspection in this conversation.
	Recoveries(context.Context) ([]conversation.Recovery, error)
	// ResumeTurn resumes the identified turn and delivers progress through the event sink.
	ResumeTurn(context.Context, string, agentruntime.EventSink) (agentruntime.Result, error)
}

type recoveriesMsg struct {
	items []conversation.Recovery
	err   error
}

func discoverRecoveries(ctx context.Context, source RecoveryConversation) tea.Cmd {
	return func() tea.Msg {
		items, err := source.Recoveries(ctx)
		return recoveriesMsg{items: items, err: err}
	}
}

// recoveryFailure adds a recovery error to the transcript for inspection.
func (model *Model) recoveryFailure(message string) {
	model.notice(blockFailure, message)
}

func (model *Model) showRecoveries(message recoveriesMsg) {
	if message.err != nil {
		model.recoveryFailure(message.err.Error())
		return
	}
	text := "No interrupted turns"
	if len(message.items) > 0 {
		var output strings.Builder
		output.WriteString("Interrupted work in this session")
		canResume := false
		for _, item := range message.items {
			output.WriteString("\n  ")
			if item.TurnID != "" {
				fmt.Fprintf(&output, "turn %s  ", item.TurnID)
				canResume = canResume || item.Phase != "unavailable"
			}
			output.WriteString(item.Phase)
			if item.Reason != "" {
				fmt.Fprintf(&output, " — %s", item.Reason)
			}
			if item.Prompt != "" {
				fmt.Fprintf(&output, "\n    %s", singleLinePreview(item.Prompt, 80))
			}
		}
		if canResume {
			output.WriteString("\nUse /resume for the current turn or /resume <turn-id> to choose one.")
		}
		text = output.String()
	}
	model.notice(blockSystem, text)
}

func (model *Model) startResume(id string) tea.Cmd {
	if model.busy {
		model.recoveryFailure("a turn is already running")
		return nil
	}
	source, ok := model.conversation.(RecoveryConversation)
	if !ok {
		model.recoveryFailure("recovery is unavailable")
		return nil
	}
	if id != "" {
		model.blocks = append(model.blocks, transcriptBlock{kind: blockSystem, content: "Resuming original turn " + id})
	}
	return model.startOperation("Recovering", func(ctx context.Context, events chan<- tea.Msg) tea.Cmd {
		return runOperationCommand(ctx, func(ctx context.Context, sink agentruntime.EventSink) (agentruntime.Result, error) {
			return source.ResumeTurn(ctx, id, sink)
		}, events, model.ctx)
	})
}
