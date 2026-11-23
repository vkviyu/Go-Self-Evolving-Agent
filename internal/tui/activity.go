package tui

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentmanager"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/render"
)

const (
	activityRefreshInterval = 2 * time.Second
	// activityIdleInterval detects background work even with an idle root.
	activityIdleInterval  = 5 * time.Second
	activityMaxRows       = 9
	activityQuickChildren = 3
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
	if model.ctx == nil || model.ctx.Err() != nil ||
		model.activityProvider == nil || model.teamID == "" || model.activityLoading {
		return nil
	}
	model.activitySequence++
	model.activityLoading = true
	return loadActivityCommand(model.ctx, model.activityProvider, model.teamID)
}

// scheduleActivityRefresh schedules the next snapshot and supersedes any tick
// that is already waiting, so one refresh chain runs at a time.
func (model *Model) scheduleActivityRefresh() tea.Cmd {
	if model.ctx == nil || model.ctx.Err() != nil ||
		model.activityProvider == nil || model.teamID == "" {
		return nil
	}
	model.activitySequence++
	sequence := model.activitySequence
	interval := activityIdleInterval
	if model.showActivity || model.busy || model.hasLiveChildren() {
		interval = activityRefreshInterval
	}
	return tea.Tick(interval, func(time.Time) tea.Msg { return activityTickMsg{sequence: sequence} })
}

func (model *Model) applyActivitySnapshot(snapshot activitySnapshot) {
	selectedAgent, hasAgent := model.selectedAgent()
	selectedMerge, hasMerge := model.selectedMerge()
	if snapshot.error != "" && snapshot.Agents == nil {
		snapshot.Agents = model.activity.Agents
	}
	snapshot.Agents = orderActivityAgents(snapshot.Agents)
	model.activity = snapshot
	model.activityLoading = false
	model.clampPanelSelection()
	for index, agent := range snapshot.Agents {
		if hasAgent && agent.ID == selectedAgent.ID {
			model.panelSelected = index
			return
		}
	}
	for index, merge := range snapshot.Merges {
		if hasMerge && merge.ID == selectedMerge.ID {
			model.panelSelected = len(snapshot.Agents) + index
			return
		}
	}
	model.panelConfirm = ""
}

// orderActivityAgents keeps a parent's children together and brings live
// siblings before finished ones so they stay visible in a short panel.
func orderActivityAgents(agents []orchestration.AgentSnapshot) []orchestration.AgentSnapshot {
	children := make(map[orchestration.AgentID][]orchestration.AgentSnapshot, len(agents))
	for _, agent := range agents {
		children[agent.ParentAgentID] = append(children[agent.ParentAgentID], agent)
	}
	ordered := make([]orchestration.AgentSnapshot, 0, len(agents))
	seen := make(map[orchestration.AgentID]bool, len(agents))
	var appendAgent func(orchestration.AgentSnapshot)
	appendAgent = func(agent orchestration.AgentSnapshot) {
		if seen[agent.ID] {
			return
		}
		seen[agent.ID] = true
		ordered = append(ordered, agent)
		siblings := children[agent.ID]
		sort.SliceStable(siblings, func(i, j int) bool {
			return !isFinishedAgent(siblings[i].State) && isFinishedAgent(siblings[j].State)
		})
		for _, child := range siblings {
			appendAgent(child)
		}
	}
	for _, root := range children[""] {
		appendAgent(root)
	}
	// Partial snapshots may omit a parent, but their children remain visible.
	for _, agent := range agents {
		appendAgent(agent)
	}
	return ordered
}

func (model *Model) toggleActivity() tea.Cmd {
	model.showActivity = !model.showActivity
	model.panelFocused = model.showActivity
	model.panelConfirm = ""
	model.resizeInput()
	if !model.showActivity {
		return nil
	}
	return model.startActivityRefresh()
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
	rows := model.activityPanelRows(width)
	lines := make([]string, len(rows))
	for index, row := range rows {
		lines[index] = ansi.Truncate(row.text, width, "…")
	}
	return strings.Join(lines, "\n")
}

// Activity rows carry their selection index through clipping so mouse and
// keyboard input always target the same Agent or merge.
type activityPanelRow struct {
	text       string
	index      int
	selectable bool
}

func (model *Model) activityPanelRows(width int) []activityPanelRow {
	summary := model.agentSummary()
	if summary == "" && !model.showActivity {
		return nil
	}
	marker := "▸ "
	if model.showActivity {
		marker = "▾ "
	}
	if summary == "" {
		summary = "Child agents 0"
	}
	if width < 32 {
		summary = strings.Replace(summary, "Child agents", "Agents", 1)
	}
	hint := "  Ctrl+G"
	label := ansi.Truncate(marker+summary, max(1, width-ansi.StringWidth(hint)), "…")
	header := activityPanelRow{text: model.styles.activityHeader.Render(ansi.Truncate(label+hint, width, ""))}

	rowLimit := activityMaxRows
	if model.ready {
		rowLimit = min(rowLimit, model.height-model.headerHeight()-model.input.Height()-4)
	}
	rows := []activityPanelRow{header}
	if !model.showActivity {
		return append(rows, model.quickAgentRows(rowLimit-1)...)
	}
	if rowLimit < 1 {
		return nil
	}
	if model.activityLoading && model.activity.updatedAt.IsZero() {
		rows = append(rows, activityPanelRow{text: model.styles.metadata.Render("  loading activity...")})
	}
	if model.activity.error != "" {
		rows = append(rows, activityPanelRow{text: model.styles.activityError.Render("  activity unavailable · " + singleLinePreview(model.activity.error, width))})
	}
	body, selectedLine := model.activityBody()
	rows = append(rows, windowRows(body, selectedLine, rowLimit-len(rows), model.styles.metadata.Render)...)
	return rows[:min(len(rows), rowLimit)]
}

// activityBody renders every panel row and returns the line of the selected
// row, or -1 when the panel has no focus.
func (model *Model) activityBody() ([]activityPanelRow, int) {
	snapshot := model.activity
	now := time.Now()
	rows := make([]activityPanelRow, 0, len(snapshot.Agents)+len(snapshot.Merges)+len(snapshot.Tasks))
	selectedLine := -1
	row := func(index int, style lipgloss.Style, text string, viewing bool) {
		prefix := "  "
		if viewing {
			prefix, style = "● ", model.styles.activityHeader
		}
		if model.panelFocused && index == model.panelSelected {
			selectedLine = len(rows)
			prefix, style = "› ", model.styles.activitySelected
		}
		rows = append(rows, activityPanelRow{index: index, selectable: true, text: style.Render(prefix + render.PlainText(text))})
	}
	for index, agent := range snapshot.Agents {
		row(index, model.styles.activityAgent, model.agentRow(agent, now, false), model.viewingAgent(agent))
	}
	for index, merge := range snapshot.Merges {
		resolution := ""
		if merge.ResolutionTaskID != "" {
			resolution = "  resolution=" + shortID(string(merge.ResolutionTaskID))
		}
		row(len(snapshot.Agents)+index, model.styles.activityMerge, "merge "+shortID(string(merge.ID))+"  "+string(merge.State)+resolution+activitySuffix(merge.Reason), false)
	}
	for _, task := range snapshot.Tasks {
		rows = append(rows, activityPanelRow{text: model.styles.activityTask.Render(render.PlainText("  task  " + shortID(string(task.ID)) + "  " + string(task.State) + "  " + strings.TrimSpace(task.Title)))})
	}
	for _, view := range snapshot.Views {
		rows = append(rows, activityPanelRow{text: model.styles.activityWorkspace.Render("  view  " + shortID(string(view.ID)) + "  " + string(view.Mode) + "  " + string(view.State))})
	}
	for _, lease := range snapshot.Leases {
		rows = append(rows, activityPanelRow{text: model.styles.activityWorkspace.Render(render.PlainText("  lease " + shortID(string(lease.ID)) + "  " + string(lease.Mode) + "  " + lease.Path))})
	}
	return rows, selectedLine
}

// Keep a small switcher visible during parallel work. Finished Agents remain
// accessible through the full panel, including the currently viewed child.
func (model *Model) quickAgentRows(limit int) []activityPanelRow {
	childLimit := min(activityQuickChildren, limit-1)
	if childLimit < 1 || model.transcripts == nil {
		return nil
	}
	agents := model.activity.Agents
	root, current := -1, -1
	children := make([]int, 0, childLimit)
	for index, agent := range agents {
		if agent.ParentAgentID == "" {
			root = index
			continue
		}
		if model.viewingAgent(agent) {
			current = index
		}
		if !isFinishedAgent(agent.State) && len(children) < childLimit {
			children = append(children, index)
		}
	}
	if current >= 0 && !slices.Contains(children, current) {
		children = append(children[:min(len(children), childLimit-1)], current)
	}
	if root < 0 || len(children) == 0 {
		return nil
	}
	now := time.Now()
	rows := make([]activityPanelRow, 0, len(children)+1)
	for _, index := range append([]int{root}, children...) {
		agent := agents[index]
		prefix, style := "○ ", model.styles.activityAgent
		if model.viewingAgent(agent) {
			prefix, style = "● ", model.styles.activityHeader
		}
		rows = append(rows, activityPanelRow{index: index, selectable: true, text: style.Render(prefix + render.PlainText(model.agentRow(agent, now, true)))})
	}
	return rows
}

func (model *Model) viewingAgent(agent orchestration.AgentSnapshot) bool {
	if model.viewer != nil {
		return model.viewer.agentID == agent.ID
	}
	return agent.ParentAgentID == ""
}

// agentRow describes one Agent: state, live activity, task, elapsed time and
// recorded tokens.
func (model *Model) agentRow(agent orchestration.AgentSnapshot, now time.Time, compact bool) string {
	if agent.ParentAgentID == "" {
		return model.rootAgentRow(agent, compact)
	}
	indent := strings.Repeat("  ", max(0, min(agent.Depth-1, 3)))
	parts := []string{indent + "↳ " + shortID(string(agent.ID)), string(agent.State)}
	if activity := activityPreview(agent.Activity, 32); activity != "" && activity != string(agent.State) {
		parts = append(parts, activity)
	}
	if elapsed := agentElapsed(agent, now); elapsed != "" {
		parts = append(parts, elapsed)
	}
	if usage, ok := model.activity.Usage[agent.ID]; ok && usage.TotalTokens > 0 {
		parts = append(parts, formatTokens(usage.TotalTokens)+" tok")
	}
	if label := model.agentAssignment(agent); label != "" {
		parts = append(parts, label)
	}
	if agent.Result != nil && isFinishedAgent(agent.State) {
		detail := agent.Result.Error
		if detail == "" {
			detail = agent.Result.Summary
		}
		if detail = activityPreview(detail, 90); detail != "" {
			parts = append(parts, "· "+detail)
		}
	}
	return strings.Join(parts, "  ")
}

// rootAgentRow describes the root Agent. The Supervisor never runs the root,
// so its durable state stays queued; this TUI drives its turns and knows
// whether one is running. Its lifetime is the session's, so it has no elapsed
// time.
func (model *Model) rootAgentRow(agent orchestration.AgentSnapshot, compact bool) string {
	state := "idle"
	if model.busy {
		state = "running"
	}
	label := "agent " + shortID(string(agent.ID)) + " (root)"
	if compact {
		label = "main"
	}
	parts := []string{label, state}
	if model.busy {
		parts = append(parts, singleLinePreview(model.stage, 40))
	}
	if usage, ok := model.activity.Usage[agent.ID]; ok && usage.TotalTokens > 0 {
		parts = append(parts, formatTokens(usage.TotalTokens)+" tok")
	}
	return strings.Join(parts, "  ")
}

func (model *Model) agentAssignment(agent orchestration.AgentSnapshot) string {
	for _, task := range model.activity.Tasks {
		if agent.AssignedTaskID != "" && task.ID == agent.AssignedTaskID {
			return fmt.Sprintf("%s (%s)", activityPreview(task.Title, 50), task.State)
		}
	}
	return activityPreview(agent.InitialPrompt, 56)
}

func activityPreview(value string, width int) string {
	// Render work is bounded even when an Agent's assignment contains a long
	// pasted document or its result contains a full tool response.
	return singleLinePreview(value[:min(len(value), 1024)], width)
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
func windowRows(lines []activityPanelRow, selected, limit int, render func(...string) string) []activityPanelRow {
	if limit <= 0 {
		return nil
	}
	if len(lines) <= limit {
		return lines
	}
	if limit == 1 {
		index := max(0, selected)
		return lines[index : index+1]
	}
	visible := limit - 1
	start := 0
	if selected >= visible {
		start = selected - visible + 1
	}
	window := slices.Clone(lines[start : start+visible])
	return append(window, activityPanelRow{text: render(fmt.Sprintf("  ... %d more", len(lines)-visible))})
}

// agentSummary describes every persisted child in the team, including those
// that finished before the current turn or before the session was reopened.
func (model *Model) agentSummary() string {
	counts := make(map[orchestration.AgentState]int)
	total := 0
	for _, agent := range model.activity.Agents {
		if agent.ParentAgentID == "" {
			continue
		}
		total++
		counts[agent.State]++
	}
	parts := make([]string, 0, 9)
	if total > 0 {
		parts = append(parts, fmt.Sprintf("Child agents %d", total))
		counts[orchestration.AgentQueued] += counts[orchestration.AgentCreated]
		for _, state := range []orchestration.AgentState{
			orchestration.AgentRunning, orchestration.AgentQueued, orchestration.AgentWaiting, orchestration.AgentCanceling,
			orchestration.AgentCompleted, orchestration.AgentFailed, orchestration.AgentCanceled,
		} {
			if count := counts[state]; count > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", count, state))
			}
		}
	}
	if pending := model.undecidedMerges(); pending > 0 {
		parts = append(parts, fmt.Sprintf("merges %d pending", pending))
	}
	return strings.Join(parts, " · ")
}

func (model *Model) hasLiveChildren() bool {
	for _, agent := range model.activity.Agents {
		if agent.ParentAgentID != "" && !isFinishedAgent(agent.State) {
			return true
		}
	}
	return false
}

func (model *Model) hasActivityOverview() bool {
	for _, agent := range model.activity.Agents {
		if agent.ParentAgentID != "" {
			return true
		}
	}
	return model.undecidedMerges() > 0
}

// Agent tool results are the first live indication that the root changed the
// team. Polling still covers transitions performed by a child or Supervisor.
func (model *Model) refreshAfterAgentTool(event agentruntime.Event) tea.Cmd {
	if event.Type != agentruntime.EventToolResult || event.Message == nil {
		return nil
	}
	name := event.Message.ToolName
	if name == "" {
		if index, ok := model.activeTools[event.Message.ToolCallID]; ok && index < len(model.blocks) {
			name = model.blocks[index].toolName
		}
	}
	switch name {
	case "agent_spawn", "agent_continue", "agent_resume", "agent_interrupt", "agent_wait", "task_reassign":
		return model.startActivityRefresh()
	}
	return nil
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
	return len(model.activityPanelRows(max(1, model.width-2)))
}
