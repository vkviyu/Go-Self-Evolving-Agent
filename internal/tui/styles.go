package tui

import "charm.land/lipgloss/v2"

type styles struct {
	brand             lipgloss.Style
	metadata          lipgloss.Style
	separator         lipgloss.Style
	userMarker        lipgloss.Style
	userPanel         lipgloss.Style
	assistant         lipgloss.Style
	reasoningPanel    lipgloss.Style
	tool              lipgloss.Style
	toolHeader        lipgloss.Style
	toolArguments     lipgloss.Style
	system            lipgloss.Style
	failure           lipgloss.Style
	approval          lipgloss.Style
	approvalReason    lipgloss.Style
	approvalOption    lipgloss.Style
	approvalSelected  lipgloss.Style
	approved          lipgloss.Style
	rejected          lipgloss.Style
	toolResult        lipgloss.Style
	status            lipgloss.Style
	ready             lipgloss.Style
	input             lipgloss.Style
	suggest           lipgloss.Style
	selection         lipgloss.Style
	activityHeader    lipgloss.Style
	activityAgent     lipgloss.Style
	activityTask      lipgloss.Style
	activityWorkspace lipgloss.Style
	activityMerge     lipgloss.Style
	activityError     lipgloss.Style
	activitySelected  lipgloss.Style
	background        lipgloss.Style
	backgroundHeader  lipgloss.Style
}

func defaultStyles() styles {
	return styles{
		brand:             lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("81")),
		metadata:          lipgloss.NewStyle().Foreground(lipgloss.Color("245")),
		separator:         lipgloss.NewStyle().Foreground(lipgloss.Color("238")),
		userMarker:        lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("81")).Background(lipgloss.Color("236")),
		userPanel:         lipgloss.NewStyle().Foreground(lipgloss.Color("252")).Background(lipgloss.Color("236")).Padding(0, 1),
		assistant:         lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("114")),
		reasoningPanel:    lipgloss.NewStyle().Foreground(lipgloss.Color("244")).Background(lipgloss.Color("235")).Padding(0, 1),
		tool:              lipgloss.NewStyle().Foreground(lipgloss.Color("183")),
		toolHeader:        lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("183")),
		toolArguments:     lipgloss.NewStyle().Foreground(lipgloss.Color("252")),
		system:            lipgloss.NewStyle().Foreground(lipgloss.Color("245")),
		failure:           lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("203")),
		approval:          lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214")),
		approvalReason:    lipgloss.NewStyle().Foreground(lipgloss.Color("214")),
		approvalOption:    lipgloss.NewStyle().Foreground(lipgloss.Color("252")),
		approvalSelected:  lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("232")).Background(lipgloss.Color("214")),
		approved:          lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("42")),
		rejected:          lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("203")),
		toolResult:        lipgloss.NewStyle().Foreground(lipgloss.Color("114")),
		status:            lipgloss.NewStyle().Foreground(lipgloss.Color("221")),
		ready:             lipgloss.NewStyle().Foreground(lipgloss.Color("245")),
		input:             lipgloss.NewStyle().Foreground(lipgloss.Color("81")),
		suggest:           lipgloss.NewStyle().Foreground(lipgloss.Color("183")),
		selection:         lipgloss.NewStyle().Background(lipgloss.Color("238")),
		activityHeader:    lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("109")),
		activityAgent:     lipgloss.NewStyle().Foreground(lipgloss.Color("151")),
		activityTask:      lipgloss.NewStyle().Foreground(lipgloss.Color("186")),
		activityWorkspace: lipgloss.NewStyle().Foreground(lipgloss.Color("110")),
		activityMerge:     lipgloss.NewStyle().Foreground(lipgloss.Color("179")),
		activityError:     lipgloss.NewStyle().Foreground(lipgloss.Color("203")),
		activitySelected:  lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("232")).Background(lipgloss.Color("109")),
		background:        lipgloss.NewStyle().Foreground(lipgloss.Color("250")),
		backgroundHeader:  lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("109")),
	}
}
