package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/app"
	"github.com/vkviyu/go-self-evolving-agent/internal/ateprocess"
	"github.com/vkviyu/go-self-evolving-agent/internal/aterequest"
	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/runtimeowner"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

type (
	// Preserve typed causes without printing saved request bodies or config.
	sessionReconcileError struct {
		message string
		cause   error
	}
	reconcileProgress struct {
		mu    sync.Mutex
		stage ateprocess.Stage
		at    time.Time
	}
)

func reconcileSession(ctx context.Context, arguments []string, stdout, stderr io.Writer) error {
	cfg, id, err := parseSessionReconcileOptions(arguments, stderr)
	if err != nil {
		return err
	}
	var progress reconcileProgress
	ctx = ateprocess.WithObserver(ctx, func(_ context.Context, event ateprocess.Progress) {
		if event.Stage == ateprocess.StageDone || event.Stage == ateprocess.StageShutdown {
			return
		}
		progress.mu.Lock()
		progress.stage, progress.at = event.Stage, event.At
		progress.mu.Unlock()
	})
	result, err := app.ReconcileSession(ctx, cfg, id)
	if err != nil {
		message := fmt.Sprintf("Session %s reconciliation failed; check existing stores and configuration", id)
		if errors.Is(err, runtimeowner.ErrOwned) {
			message = fmt.Sprintf("Session %s reconciliation blocked: runtime is owned", id)
		} else if errors.Is(err, aterequest.ErrUnknown) {
			message = fmt.Sprintf("Session %s outcome remains unknown; original pending requests are retained", id)
		} else if errors.Is(err, context.DeadlineExceeded) {
			message = fmt.Sprintf("Session %s reconciliation timed out; completion has not been confirmed", id)
		}
		progress.mu.Lock()
		if progress.stage != "" {
			message += fmt.Sprintf(" (ATE %s, %s elapsed)", progress.stage, time.Since(progress.at).Round(time.Second))
		}
		progress.mu.Unlock()
		return sessionReconcileError{message: message, cause: err}
	}
	return writeSessionJSON(stdout, result)
}

// Parse before loading configuration so help and malformed arguments cannot
// open authority. Like chat, load user/profile/project/environment config, then
// apply flags; no provider validation or model construction is involved.
func parseSessionReconcileOptions(arguments []string, stderr io.Writer) (cfg config.Config, id session.ID, err error) {
	flags := flag.NewFlagSet("session reconcile", flag.ContinueOnError)
	// flag errors echo user input. Only fixed diagnostics reach the terminal.
	flags.SetOutput(io.Discard)
	profile := flags.String("profile", "", "configuration profile")
	flags.StringVar(profile, "p", "", "configuration profile")
	data := flags.String("data-dir", "", "existing runtime database directory")
	workspace := flags.String("workspace", "", "existing workspace directory")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "usage: gsea session reconcile <session-id> [--data-dir DIR] [--workspace DIR] [--profile NAME|-p NAME]")
	}
	leadingID := ""
	if len(arguments) > 0 && !strings.HasPrefix(arguments[0], "-") {
		leadingID, arguments = arguments[0], arguments[1:]
	}
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return cfg, "", err
		}
		return cfg, "", errors.New("invalid session reconcile flags; use --help")
	}
	if flags.NArg() > 1 || (leadingID != "" && flags.NArg() != 0) {
		return cfg, "", errors.New("session reconcile requires exactly one Session ID")
	}
	if leadingID == "" && flags.NArg() == 1 {
		leadingID = flags.Arg(0)
	}
	if !sessionIDPattern.MatchString(leadingID) {
		return cfg, "", errors.New("session reconcile requires one Session ID: 1–128 letters, digits, '.', '_' or '-', starting with a letter or digit")
	}
	var invalid bool
	flags.Visit(func(f *flag.Flag) {
		value := f.Value.String()
		if strings.TrimSpace(value) == "" || len(value) > 4096 || strings.ContainsAny(value, "\x00\r\n") {
			invalid = true
		}
	})
	if invalid {
		return cfg, "", errors.New("session reconcile flags require nonblank, bounded values without NUL or line breaks")
	}
	cfg, err = config.Load(config.LoadOptions{Profile: *profile})
	if err != nil {
		return cfg, "", sessionReconcileError{message: "session reconcile configuration could not be loaded", cause: err}
	}
	if *data != "" {
		cfg.DataDirectory = *data
	}
	if *workspace != "" {
		cfg.WorkspaceDirectory = *workspace
	}
	return cfg, session.ID(leadingID), nil
}

func (e sessionReconcileError) Error() string { return e.message }
func (e sessionReconcileError) Unwrap() error { return e.cause }
