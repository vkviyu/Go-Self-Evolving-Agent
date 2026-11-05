package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentmanager"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/render"
)

const (
	activityRefreshInterval = 2 * time.Second
	// activityIdleInterval keeps the status summary current while the panel
	// is hidden.
	activityIdleInterval = 5 * time.Second
	activityMaxRows      = 9
)

// activitySnapshot is intentionally a presentation snapshot. It keeps the
// TUI independent from the Supervisor's mutable maps and lifecycle locks.
type activitySnapshot struct {
	agentmanager.TeamSnapshot
	error     string
	updatedAt time.Time
}

func loadActivityCommand(ctx context.Context, provider agentmanager.TeamSnapshotReader, teamID orchestration.TeamID) tea.Cmd {
	return func() tea.Msg {
		if provider == nil || teamID == "" {
			return activityRefreshMsg{snapshot: activitySnapshot{updatedAt: time.Now().UTC()}}
		}
		loadContext, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		snapshot := activitySnapshot{updatedAt: time.Now().UTC()}
		teamSnapshot, err := provider.LoadTeamSnapshot(loadContext, teamID)
		snapshot.TeamSnapshot = teamSnapshot
		if err != nil {
			snapshot.error = err.Error()
		}
		return activityRefreshMsg{snapshot: snapshot}
	}
}

// startActivityRefresh loads the first snapshot; each loaded snapshot then
// schedules the next refresh.
func (model *Model) startActivityRefresh() tea.Cmd {
	if model.activityProvider == nil || model.teamID == "" {
		return nil
	}
	model.activityLoading = true
	return loadActivityCommand(model.ctx, model.activityProvider, model.teamID)
}

// scheduleActivityRefresh schedules the next snapshot and supersedes any tick
// that is already waiting, so one refresh chain runs at a time.
func (model *Model) scheduleActivityRefresh() tea.Cmd {
	if model.activityProvider == nil || model.teamID == "" {
		return nil
	}
	model.activitySequence++
	sequence := model.activitySequence
	interval := activityIdleInterval
	if model.showActivity {
		interval = activityRefreshInterval
	}
	return tea.Tick(interval, func(time.Time) tea.Msg { return activityTickMsg{sequence: sequence} })
}

func (model *Model) toggleActivity() tea.Cmd {
	model.showActivity = !model.showActivity
	model.panelFocused = model.showActivity
	model.panelConfirm = ""
	model.resizeInput()
	model.refreshTranscript(true)
	if !model.showActivity || model.activityProvider == nil {
		return nil
	}
	model.activityLoading = true
	return loadActivityCommand(model.ctx, model.activityProvider, model.teamID)
}

// openAgentsPanel shows and focuses the /agents panel.
func (model *Model) openAgentsPanel() tea.Cmd {
	if model.showActivity {
		model.panelFocused = true
		model.refreshTranscript(false)
		return nil
	}
	return model.toggleActivity()
}

func (model *Model) renderActivityPanel(width int) string {
	if !model.showActivity {
		return ""
	}
	rowLimit := activityMaxRows
	if model.ready {
		rowLimit = min(rowLimit, model.height-model.headerHeight()-model.input.Height()-4)
		if rowLimit < 1 {
			return ""
		}
	}
	snapshot := model.activity
	header := fmt.Sprintf("agents %d  tasks %d  views %d  merges %d", len(snapshot.Agents), len(snapshot.Tasks), len(snapshot.Views), len(snapshot.Merges))
	if summary := model.agentSummary(); summary != "" {
		header += "  · " + summary
	}
	lines := []string{model.styles.activityHeader.Render(header)}
	if model.activityLoading && snapshot.updatedAt.IsZero() {
		lines = append(lines, model.styles.metadata.Render("  loading activity..."))
	}
	body, selectedLine := model.activityBody()
	lines = append(lines, windowRows(body, selectedLine, max(0, rowLimit-len(lines)), model.styles.metadata.Render)...)
	lines = lines[:min(len(lines), rowLimit)]
	for index := range lines {
		if ansi.StringWidth(ansi.Strip(lines[index])) > width {
			lines[index] = ansi.Truncate(lines[index], width, "...")
		}
	}
	return strings.Join(lines, "\n")
}

// activityBody renders every panel row and returns the line of the selected
// row, or -1 when the panel has no focus.
func (model *Model) activityBody() ([]string, int) {
	snapshot := model.activity
	now := time.Now()
	lines := make([]string, 0, len(snapshot.Agents)+len(snapshot.Merges)+len(snapshot.Tasks))
	selectedLine := -1
	row := func(index int, style lipgloss.Style, text string) {
		text = render.PlainText(text)
		if model.panelFocused && index == model.panelSelected {
			selectedLine = len(lines)
			lines = append(lines, model.styles.activitySelected.Render("› "+text))
			return
		}
		lines = append(lines, style.Render("  "+text))
	}
	for index, agent := range snapshot.Agents {
		row(index, model.styles.activityAgent, model.agentRow(agent, now))
	}
	for index, merge := range snapshot.Merges {
		resolution := ""
		if merge.ResolutionTaskID != "" {
			resolution = "  resolution=" + shortID(string(merge.ResolutionTaskID))
		}
		row(len(snapshot.Agents)+index, model.styles.activityMerge, "merge "+shortID(string(merge.ID))+"  "+string(merge.State)+resolution+activitySuffix(merge.Reason))
	}
	for _, task := range snapshot.Tasks {
		lines = append(lines, model.styles.activityTask.Render(render.PlainText("  task  "+shortID(string(task.ID))+"  "+string(task.State)+"  "+strings.TrimSpace(task.Title))))
	}
	for _, view := range snapshot.Views {
		lines = append(lines, model.styles.activityWorkspace.Render("  view  "+shortID(string(view.ID))+"  "+string(view.Mode)+"  "+string(view.State)))
	}
	for _, lease := range snapshot.Leases {
		lines = append(lines, model.styles.activityWorkspace.Render(render.PlainText("  lease "+shortID(string(lease.ID))+"  "+string(lease.Mode)+"  "+lease.Path)))
	}
	if snapshot.error != "" {
		lines = append(lines, model.styles.activityError.Render("  activity unavailable: "+render.PlainText(snapshot.error)))
	}
	return lines, selectedLine
}

// agentRow describes one Agent: state, live activity, task, elapsed time and
// recorded tokens.
func (model *Model) agentRow(agent orchestration.AgentSnapshot, now time.Time) string {
	if agent.ParentAgentID == "" {
		return model.rootAgentRow(agent)
	}
	parts := []string{"agent " + shortID(string(agent.ID)), string(agent.State)}
	if activity := strings.TrimSpace(agent.Activity); activity != "" {
		parts = append(parts, activity)
	}
	if title := model.taskTitle(agent.AssignedTaskID); title != "" {
		parts = append(parts, "task "+title)
	}
	if elapsed := agentElapsed(agent, now); elapsed != "" {
		parts = append(parts, elapsed)
	}
	if usage, ok := model.activity.Usage[agent.ID]; ok && usage.TotalTokens > 0 {
		parts = append(parts, formatTokens(usage.TotalTokens)+" tok")
	}
	return strings.Join(parts, "  ")
}

// rootAgentRow describes the root Agent. The Supervisor never runs the root,
// so its durable state stays queued; this TUI drives its turns and knows
// whether one is running. Its lifetime is the session's, so it has no elapsed
// time.
func (model *Model) rootAgentRow(agent orchestration.AgentSnapshot) string {
	state := "idle"
	if model.busy {
		state = "running"
	}
	parts := []string{"agent " + shortID(string(agent.ID)) + " (root)", state}
	if usage, ok := model.activity.Usage[agent.ID]; ok && usage.TotalTokens > 0 {
		parts = append(parts, formatTokens(usage.TotalTokens)+" tok")
	}
	return strings.Join(parts, "  ")
}

func (model *Model) taskTitle(taskID orchestration.TaskID) string {
	if taskID == "" {
		return ""
	}
	for _, task := range model.activity.Tasks {
		if task.ID == taskID {
			return strings.TrimSpace(task.Title)
		}
	}
	return ""
}

// agentElapsed is the time an active Agent has existed, or the time a
// finished Agent took.
func agentElapsed(agent orchestration.AgentSnapshot, now time.Time) string {
	if agent.CreatedAt.IsZero() {
		return ""
	}
	end := now
	if isFinishedAgent(agent.State) && !agent.UpdatedAt.IsZero() {
		end = agent.UpdatedAt
	}
	elapsed := end.Sub(agent.CreatedAt)
	if elapsed < 0 {
		return ""
	}
	return formatElapsed(elapsed)
}

func formatElapsed(elapsed time.Duration) string {
	seconds := int(elapsed / time.Second)
	switch {
	case seconds < 60:
		return fmt.Sprintf("%ds", seconds)
	case seconds < 3600:
		return fmt.Sprintf("%dm%02ds", seconds/60, seconds%60)
	default:
		return fmt.Sprintf("%dh%02dm", seconds/3600, seconds%3600/60)
	}
}

func isFinishedAgent(state orchestration.AgentState) bool {
	return state == orchestration.AgentCompleted || state == orchestration.AgentFailed || state == orchestration.AgentCanceled
}

// windowRows fits lines into limit rows. When they overflow, the window keeps
// the selected line visible and its last row counts the hidden lines.
func windowRows(lines []string, selected, limit int, render func(...string) string) []string {
	if limit <= 0 {
		return nil
	}
	if len(lines) <= limit {
		return lines
	}
	visible := limit - 1
	start := 0
	if selected >= visible {
		start = selected - visible + 1
	}
	window := append([]string(nil), lines[start:start+visible]...)
	return append(window, render(fmt.Sprintf("  ... %d more", len(lines)-visible)))
}

// agentSummary counts the active child Agents and the merges that wait for a
// decision, such as "agents 2 running 1 queued".
func (model *Model) agentSummary() string {
	var running, waiting, queued int
	for _, agent := range model.activity.Agents {
		if agent.ParentAgentID == "" {
			continue
		}
		switch agent.State {
		case orchestration.AgentRunning, orchestration.AgentCanceling:
			running++
		case orchestration.AgentWaiting:
			waiting++
		case orchestration.AgentCreated, orchestration.AgentQueued:
			queued++
		}
	}
	parts := make([]string, 0, 4)
	for _, count := range []struct {
		value int
		label string
	}{{running, "running"}, {waiting, "waiting"}, {queued, "queued"}} {
		if count.value > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", count.value, count.label))
		}
	}
	summary := ""
	if len(parts) > 0 {
		summary = "agents " + strings.Join(parts, " ")
	}
	if pending := model.undecidedMerges(); pending > 0 {
		if summary != "" {
			summary += "  "
		}
		summary += fmt.Sprintf("merges %d pending", pending)
	}
	return summary
}

func (model *Model) undecidedMerges() int {
	count := 0
	for _, merge := range model.activity.Merges {
		if isUndecidedMerge(merge.State) {
			count++
		}
	}
	return count
}

func activitySuffix(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	return "  " + value
}

func shortID(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 16 {
		return value
	}
	return value[:8] + "..." + value[len(value)-4:]
}

func (model *Model) activityHeight() int {
	if !model.showActivity {
		return 0
	}
	panel := model.renderActivityPanel(max(1, model.width-2))
	if panel == "" {
		return 0
	}
	return lipgloss.Height(panel)
}
