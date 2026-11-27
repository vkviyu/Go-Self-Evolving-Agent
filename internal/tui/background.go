package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/conversation"
)

const (
	backgroundCheckInterval = 2 * time.Second
	// minimumWakeInterval spaces wake turns so a burst of child results
	// becomes one turn instead of many.
	minimumWakeInterval = 5 * time.Second
	// maximumConsecutiveWakes bounds how often the root Agent wakes itself
	// before the user speaks again.
	maximumConsecutiveWakes = 3
	backgroundPreviewLines  = 6
)

// isBackgroundInput reports whether a user-role message is a block of Agent
// messages the runtime added, rather than text the user typed.
func isBackgroundInput(content string) bool {
	return strings.HasPrefix(content, agentruntime.BackgroundInputHeader)
}

// scheduleBackgroundCheck starts the next idle check. Exactly one check chain
// runs: every check schedules its successor once it has finished.
func (model *Model) scheduleBackgroundCheck() tea.Cmd {
	if model.ctx == nil || model.ctx.Err() != nil || model.background == nil {
		return nil
	}
	return tea.Tick(backgroundCheckInterval, func(time.Time) tea.Msg { return backgroundTickMsg{} })
}

// wakeAllowed applies the wake policy: the root Agent wakes only while it is
// idle, nothing waits for the user, and the wake budget is not used up.
func (model *Model) wakeAllowed() bool {
	switch {
	case model.background == nil, model.busy, model.approvalPending != nil, model.wakeSuppressed:
		return false
	case model.consecutiveWakes >= maximumConsecutiveWakes:
		return false
	case strings.TrimSpace(model.input.Value()) != "":
		// The user is writing; their turn will receive the messages.
		return false
	case !model.lastWake.IsZero() && time.Since(model.lastWake) < minimumWakeInterval:
		return false
	}
	return true
}

// checkBackground counts the available Agent messages when a wake is allowed.
func (model *Model) checkBackground() tea.Cmd {
	if model.ctx == nil || model.ctx.Err() != nil {
		return nil
	}
	if !model.wakeAllowed() {
		return model.scheduleBackgroundCheck()
	}
	ctx, background := model.ctx, model.background
	return func() tea.Msg {
		checkContext, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		count, err := background.PendingBackground(checkContext)
		return backgroundPendingMsg{count: count, err: err}
	}
}

// handleBackgroundPending starts a wake turn when messages are waiting and
// the policy still allows it; the state may have changed during the count.
func (model *Model) handleBackgroundPending(message backgroundPendingMsg) tea.Cmd {
	next := model.scheduleBackgroundCheck()
	if message.err != nil || message.count == 0 || !model.wakeAllowed() {
		return next
	}
	return tea.Batch(model.startWake(message.count), next)
}

// startWake runs a wake turn and shows that Agent messages opened it.
func (model *Model) startWake(count int) tea.Cmd {
	model.lastWake = time.Now()
	model.consecutiveWakes++
	label := "1 Agent message arrived"
	if count != 1 {
		label = fmt.Sprintf("%d Agent messages arrived", count)
	}
	model.blocks = append(model.blocks, transcriptBlock{kind: blockBackground, content: label})
	model.backgroundBlock = len(model.blocks) - 1
	background := model.background
	command := model.startOperation("Reading Agent results", func(ctx context.Context, events chan<- tea.Msg) tea.Cmd {
		return runOperationCommand(ctx, background.RunBackgroundTurn, events, model.ctx)
	})
	model.backgroundRunning = true
	// The messages usually mean a child finished; refresh the Agent summary
	// now instead of at the next idle refresh.
	return tea.Batch(command, model.startActivityRefresh())
}

// finishWake settles wake-specific state after a turn. It reports whether
// err is a skipped wake that leaves no trace in the transcript.
func (model *Model) finishWake(err error) bool {
	running := model.backgroundRunning
	model.backgroundRunning = false
	block := model.backgroundBlock
	model.backgroundBlock = -1
	if !running {
		return false
	}
	if !errors.Is(err, agentruntime.ErrNoBackgroundInput) && !errors.Is(err, conversation.ErrBackgroundBudgetExhausted) {
		return false
	}
	// Another turn claimed the messages, or the team has no budget left.
	if block >= 0 && block == len(model.blocks)-1 && model.blocks[block].kind == blockBackground {
		model.blocks = model.blocks[:block]
	}
	return true
}

// resetWakeBudget restores the wake budget when the user speaks again.
func (model *Model) resetWakeBudget() {
	model.wakeSuppressed = false
	model.consecutiveWakes = 0
}

// renderBackgroundBody shows the Agent messages of a background block, one
// line each, below the untrusted-data header.
func (model *Model) renderBackgroundBody(content string, width int) string {
	header := "Agent messages"
	body := strings.TrimSpace(strings.TrimPrefix(content, agentruntime.BackgroundInputHeader))
	if !isBackgroundInput(content) {
		header, body = content, ""
	}
	lines := []string{model.styles.backgroundHeader.Render(ansi.Truncate(header, max(1, width), "..."))}
	if body == "" {
		return strings.Join(lines, "\n")
	}
	entries := strings.Split(body, "\n")
	for index, entry := range entries {
		if index == backgroundPreviewLines {
			lines = append(lines, model.styles.metadata.Render(fmt.Sprintf("  ... %d more", len(entries)-index)))
			break
		}
		entry = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(entry), "- "))
		lines = append(lines, model.styles.background.Render(ansi.Truncate("  "+entry, max(1, width), "...")))
	}
	return strings.Join(lines, "\n")
}
