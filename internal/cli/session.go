package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/sessionregistry"
)

func runSession(ctx context.Context, arguments []string, stdout, stderr io.Writer) error {
	if len(arguments) == 0 {
		return errors.New("session requires a subcommand: create, list, show or remove")
	}
	resolved, err := config.Default()
	if err != nil {
		return err
	}
	switch arguments[0] {
	case "create":
		return createSession(ctx, resolved, arguments[1:], stdout, stderr)
	case "list":
		return listSessions(ctx, resolved, arguments[1:], stdout, stderr)
	case "show":
		return showSession(ctx, resolved, arguments[1:], stdout, stderr)
	case "remove":
		return removeSession(ctx, resolved, arguments[1:], stdout, stderr)
	case "help", "-h", "--help":
		printSessionUsage(stdout)
		return nil
	default:
		return fmt.Errorf("unknown session subcommand %q", arguments[0])
	}
}

func createSession(ctx context.Context, resolved config.Config, arguments []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("session create", flag.ContinueOnError)
	flags.SetOutput(stderr)
	name := flags.String("name", "", "human-readable Session name")
	id := flags.String("id", "", "stable Session ID; omitted generates one")
	workspace := flags.String("workspace", "", "existing workspace directory bound to the Session")
	model := flags.String("model", resolved.Model.Name, "model name recorded in the Session catalog")
	provider := flags.String("provider", resolved.Model.Provider, "model provider recorded in the Session catalog")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("session create does not accept positional arguments")
	}
	if strings.TrimSpace(*name) == "" {
		return errors.New("session create requires --name")
	}
	if strings.TrimSpace(*workspace) == "" {
		return errors.New("session create requires --workspace")
	}
	workspacePath, err := filepath.Abs(*workspace)
	if err != nil {
		return fmt.Errorf("resolve workspace: %w", err)
	}
	info, err := os.Stat(workspacePath)
	if err != nil {
		return fmt.Errorf("inspect workspace: %w", err)
	}
	if !info.IsDir() {
		return errors.New("session workspace must be a directory")
	}
	sessionID := strings.TrimSpace(*id)
	if sessionID == "" {
		generated, err := newSessionID()
		if err != nil {
			return err
		}
		sessionID = string(generated)
	}
	if !sessionIDPattern.MatchString(sessionID) {
		return errors.New("Session ID must start with an alphanumeric character and contain only letters, digits, '.', '_' or '-'")
	}
	runtimeDirectory := filepath.Join(resolved.HomeDirectory, "sessions", sessionID)
	item := sessionregistry.Session{
		ID: sessionID, Name: strings.TrimSpace(*name),
		Workspace: workspacePath, Runtime: runtimeDirectory,
		DataDirectory: filepath.Join(runtimeDirectory, "data"),
		TempDirectory: filepath.Join(runtimeDirectory, "tmp"),
		ToolState:     filepath.Join(runtimeDirectory, "tool-state"),
		Model:         strings.TrimSpace(*model), Provider: strings.TrimSpace(*provider),
	}
	registry, err := sessionregistry.Open(ctx, resolved.SessionRegistryPath())
	if err != nil {
		return err
	}
	defer registry.Close()
	if err := registry.Create(ctx, item); err != nil {
		return err
	}
	created, err := registry.Get(ctx, item.ID)
	if err != nil {
		return err
	}
	return writeSessionJSON(stdout, created)
}

func listSessions(ctx context.Context, resolved config.Config, arguments []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("session list", flag.ContinueOnError)
	flags.SetOutput(stderr)
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("session list does not accept positional arguments")
	}
	registry, err := sessionregistry.Open(ctx, resolved.SessionRegistryPath())
	if err != nil {
		return err
	}
	defer registry.Close()
	items, err := registry.List(ctx)
	if err != nil {
		return err
	}
	return writeSessionJSON(stdout, items)
}

func showSession(ctx context.Context, resolved config.Config, arguments []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("session show", flag.ContinueOnError)
	flags.SetOutput(stderr)
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("session show requires exactly one Session ID")
	}
	registry, err := sessionregistry.Open(ctx, resolved.SessionRegistryPath())
	if err != nil {
		return err
	}
	defer registry.Close()
	item, err := registry.ReconcileStopped(ctx, flags.Arg(0))
	if err != nil {
		return err
	}
	return writeSessionJSON(stdout, item)
}

func removeSession(ctx context.Context, resolved config.Config, arguments []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("session remove", flag.ContinueOnError)
	flags.SetOutput(stderr)
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("session remove requires exactly one Session ID")
	}
	registry, err := sessionregistry.Open(ctx, resolved.SessionRegistryPath())
	if err != nil {
		return err
	}
	defer registry.Close()
	item, err := registry.ReconcileStopped(ctx, flags.Arg(0))
	if err != nil {
		return err
	}
	if item.State == sessionregistry.StateRunning {
		return errors.New("cannot remove a running Session")
	}
	if err := registry.Remove(ctx, item.ID); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "removed Session %s\n", item.ID)
	return err
}

func writeSessionJSON(writer io.Writer, value any) error {
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func printSessionUsage(writer io.Writer) {
	fmt.Fprintln(writer, "usage: local-agent session <create|list|show|remove>")
	fmt.Fprintln(writer, "  create  register a Session and allocate central runtime resources")
	fmt.Fprintln(writer, "  list    list registered Sessions")
	fmt.Fprintln(writer, "  show    inspect one Session")
	fmt.Fprintln(writer, "  remove  remove a stopped Session from the catalog")
}
