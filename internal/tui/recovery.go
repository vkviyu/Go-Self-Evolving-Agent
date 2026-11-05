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
	Recoveries(context.Context) ([]conversation.Recovery, error)
	ResumeTurn(context.Context, string, agentruntime.EventSink) (agentruntime.Result, error)
}

type recoveriesMsg struct {
	items    []conversation.Recovery
	err      error
	explicit bool
}

func discoverRecoveries(ctx context.Context, source RecoveryConversation, explicit bool) tea.Cmd {
	return func() tea.Msg {
		items, err := source.Recoveries(ctx)
		return recoveriesMsg{items: items, err: err, explicit: explicit}
	}
}

func (model *Model) recoveryFailure(message string) {
	model.notice(blockFailure, message)
}

func (model *Model) showRecoveries(message recoveriesMsg) {
	if message.err != nil {
		model.recoveryFailure(message.err.Error())
		return
	}
	if len(message.items) == 0 && !message.explicit {
		return
	}
	text := "No interrupted turns"
	if len(message.items) > 0 {
		var output strings.Builder
		output.WriteString("Interrupted work (not executed automatically):\n")
		for _, item := range message.items {
			fmt.Fprintf(&output, "%s  goal=%s  %s", item.TurnID, item.GoalID, item.Phase)
			if item.Reason != "" {
				fmt.Fprintf(&output, " — %s", item.Reason)
			}
			output.WriteByte('\n')
		}
		output.WriteString("Use /resume <turn-id>; missing checkpoints require manual inspection.")
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
	model.blocks = append(model.blocks, transcriptBlock{kind: blockSystem, content: "Resuming original turn " + id})
	return model.startOperation("Recovering", func(ctx context.Context, events chan<- tea.Msg) tea.Cmd {
		return runOperationCommand(ctx, func(ctx context.Context, sink agentruntime.EventSink) (agentruntime.Result, error) {
			return source.ResumeTurn(ctx, id, sink)
		}, events, model.ctx)
	})
}
