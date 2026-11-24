package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"
	"unicode"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/vkviyu/go-self-evolving-agent/internal/app"
	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/sessionregistry"
)

func runResume(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, deps dependencies) error {
	resolved, err := loadChatConfig(args)
	if err != nil {
		return err
	}
	options, err := parseConversationOptions(args, resolved, stderr, true)
	if err != nil {
		return err
	}
	if !options.sessionExplicit {
		items, err := discoverChatSessions(ctx, resolved, options)
		if err != nil {
			return err
		}
		terminal := isTerminal(stdin) && isTerminal(stdout)
		if options.resumeList || (!terminal && !options.resumeLast) {
			return printChatSessions(stdout, items)
		}
		if len(items) == 0 {
			return errors.New("no sessions in this workspace; use resume --all to include other workspaces and older history")
		}
		selected := items[0]
		if !options.resumeLast {
			selected, err = pickChatSession(ctx, stdin, stdout, items)
			if err != nil || selected.ID == "" {
				return err
			}
		}
		options.sessionID = session.ID(selected.ID)
		options.sessionExplicit = true
		options.dataDirectory = selected.DataDirectory
		options.dataExplicit = true
	}
	return runConfiguredChat(ctx, resolved, options, stdin, stdout, stderr, deps, nil)
}

func discoverChatSessions(ctx context.Context, resolved config.Config, options chatOptions) ([]sessionregistry.Session, error) {
	workspace, err := workspaceForChat(options.workspaceDirectory)
	if err != nil {
		return nil, err
	}
	entries, histories, err := app.ReadChatCatalog(ctx, resolved, "", true)
	if err != nil {
		return nil, err
	}
	registered := make(map[string]sessionregistry.Session, len(entries))
	for _, item := range entries {
		registered[item.ID] = item
	}
	result := make([]sessionregistry.Session, 0, len(histories))
	for _, history := range histories {
		id := string(history.ID)
		if !sessionIDPattern.MatchString(id) {
			continue
		}
		item, ok := registered[id]
		if ok {
			if item.DataDirectory == "" {
				continue
			}
			item.DataDirectory = canonicalDirectory(item.DataDirectory)
			if options.dataExplicit && item.DataDirectory != chatDataDirectory(resolved, options) {
				continue
			}
			if item.Name == "" || item.Name == id {
				item.Name = history.Prompt
			}
			if history.UpdatedAt.After(item.UpdatedAt) {
				item.UpdatedAt = history.UpdatedAt
			}
		} else {
			item = historySession(chatDataDirectory(resolved, options), history)
		}
		if !workspaceMatches(item.Workspace, workspace, options.resumeAll) {
			continue
		}
		result = append(result, item)
	}
	slices.SortFunc(result, func(a, b sessionregistry.Session) int {
		if order := b.UpdatedAt.Compare(a.UpdatedAt); order != 0 {
			return order
		}
		if order := strings.Compare(a.ID, b.ID); order != 0 {
			return order
		}
		return strings.Compare(a.DataDirectory, b.DataDirectory)
	})
	return result, nil
}

func workspaceMatches(recorded, current string, all bool) bool {
	if all {
		return true
	}
	if recorded == "" {
		return false
	}
	rel, err := filepath.Rel(canonicalDirectory(recorded), current)
	return err == nil && filepath.IsLocal(rel)
}

type resumeItem struct{ sessionregistry.Session }

func (item resumeItem) Title() string {
	if title := terminalLabel(item.Name); title != "" {
		return title
	}
	return item.ID
}

func (item resumeItem) Description() string {
	workspace := terminalLabel(item.Workspace)
	if workspace == "" {
		workspace = "workspace unrecorded"
	}
	return fmt.Sprintf("%s · %s · %s · %s", item.UpdatedAt.Local().Format("2006-01-02 15:04"), item.State, workspace, item.ID)
}

func terminalLabel(value string) string {
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return ' '
		}
		return r
	}, ansi.Strip(value))
	runes := []rune(strings.Join(strings.Fields(value), " "))
	if len(runes) > 140 {
		return string(runes[:139]) + "…"
	}
	return string(runes)
}

func printChatSessions(output io.Writer, sessions []sessionregistry.Session) error {
	if len(sessions) == 0 {
		_, err := fmt.Fprintln(output, "No sessions in this workspace. Use resume --list --all to include other workspaces and older history.")
		return err
	}
	writer := tabwriter.NewWriter(output, 0, 0, 2, ' ', 0)
	fmt.Fprintln(writer, "UPDATED\tSTATE\tSESSION\tWORKSPACE\tTITLE")
	for _, item := range sessions {
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n", item.UpdatedAt.Local().Format("2006-01-02 15:04"), item.State, item.ID, terminalLabel(item.Workspace), (resumeItem{item}).Title())
	}
	return writer.Flush()
}

type resumePicker struct {
	items    []resumeItem
	matches  []int
	filter   textinput.Model
	cursor   int
	width    int
	height   int
	selected sessionregistry.Session
}

func pickChatSession(ctx context.Context, stdin io.Reader, stdout io.Writer, sessions []sessionregistry.Session) (sessionregistry.Session, error) {
	picker := resumePicker{width: 80, height: 22, filter: textinput.New()}
	picker.filter.Placeholder = "Search by title, workspace, or session ID"
	for _, item := range sessions {
		picker.items = append(picker.items, resumeItem{item})
	}
	picker.refilter()
	picker.filter.Focus()
	model, err := tea.NewProgram(picker, tea.WithContext(ctx), tea.WithInput(stdin), tea.WithOutput(stdout), tea.WithAltScreen()).Run()
	if err != nil {
		return sessionregistry.Session{}, err
	}
	return model.(resumePicker).selected, nil
}

func (picker *resumePicker) refilter() {
	query := strings.ToLower(strings.TrimSpace(picker.filter.Value()))
	picker.matches = picker.matches[:0]
	for index, item := range picker.items {
		if strings.Contains(strings.ToLower(item.Title()+" "+item.Description()), query) {
			picker.matches = append(picker.matches, index)
		}
	}
	picker.cursor = 0
}

func (picker resumePicker) Init() tea.Cmd { return textinput.Blink }

func (picker resumePicker) View() string {
	var view strings.Builder
	fmt.Fprintln(&view, "Resume a previous session")
	fmt.Fprintln(&view, picker.filter.View())
	count := max(1, (picker.height-5)/2)
	start := max(0, picker.cursor-count+1)
	for index := start; index < min(start+count, len(picker.matches)); index++ {
		item := picker.items[picker.matches[index]]
		marker := "  "
		if index == picker.cursor {
			marker = "› "
		}
		fmt.Fprintln(&view, marker+ansi.Truncate(item.Title(), max(1, picker.width-2), "…"))
		fmt.Fprintln(&view, "  "+ansi.Truncate(item.Description(), max(1, picker.width-2), "…"))
	}
	if len(picker.matches) == 0 {
		fmt.Fprintln(&view, "No matching sessions")
	}
	fmt.Fprint(&view, "↑/↓ select · Enter resume · Esc close")
	return view.String()
}

func (picker resumePicker) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case tea.WindowSizeMsg:
		picker.width, picker.height = message.Width, message.Height
		picker.filter.Width = max(1, message.Width-3)
	case tea.KeyMsg:
		switch message.Type {
		case tea.KeyEsc, tea.KeyCtrlC:
			return picker, tea.Quit
		case tea.KeyEnter:
			if len(picker.matches) > 0 {
				picker.selected = picker.items[picker.matches[picker.cursor]].Session
				return picker, tea.Quit
			}
		case tea.KeyUp:
			picker.cursor = max(0, picker.cursor-1)
			return picker, nil
		case tea.KeyDown:
			picker.cursor = min(picker.cursor+1, max(0, len(picker.matches)-1))
			return picker, nil
		}
	}
	previous := picker.filter.Value()
	var command tea.Cmd
	picker.filter, command = picker.filter.Update(message)
	if picker.filter.Value() != previous {
		picker.refilter()
	}
	return picker, command
}
