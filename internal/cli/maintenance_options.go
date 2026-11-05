package cli

import (
	"errors"
	"flag"
	"io"

	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

type maintenanceOptions struct {
	config    config.Config
	action    string
	sessionID session.ID
	planID    string
	confirm   bool
}

// parseMaintenanceOptions consumes flags and read-only defaults, never stdin
// or the owned database. Conflicting operations cannot reach execution.
func parseMaintenanceOptions(args []string, stderr io.Writer) (options maintenanceOptions, err error) {
	if len(args) == 0 {
		return options, errors.New("maintenance requires plan, archive or compact")
	}
	action := args[0]
	if action != "plan" && action != "archive" && action != "compact" {
		return options, errors.New("unknown maintenance operation")
	}
	profile, _, err := profileArgument(args[1:])
	if err != nil {
		return options, err
	}
	cfg, err := config.Load(config.LoadOptions{Profile: profile})
	if err != nil {
		return options, err
	}
	flags := flag.NewFlagSet("maintenance "+action, flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&profile, "profile", profile, "configuration profile")
	flags.StringVar(&profile, "p", profile, "configuration profile")
	flags.StringVar(&cfg.DataDirectory, "data-dir", cfg.DataDirectory, "owned database directory")
	id := flags.String("session", "", "exact session to inspect or archive")
	planID := flags.String("plan-id", "", "reviewed preview plan ID")
	confirm := flags.Bool("confirm", false, "confirm closing recovery (not undoing effects), or compacting already-freed pages")
	if err := flags.Parse(args[1:]); err != nil {
		return options, err
	}
	if flags.NArg() != 0 {
		return options, errors.New("maintenance accepts no positional arguments")
	}
	switch action {
	case "plan":
		if *confirm || *planID != "" || !sessionIDPattern.MatchString(*id) {
			return options, errors.New("plan requires --session only; it does not apply changes")
		}
	case "archive":
		if !*confirm || *planID == "" || !sessionIDPattern.MatchString(*id) {
			return options, errors.New("archive requires --session, --plan-id and --confirm")
		}
	case "compact":
		if !*confirm || *id != "" || *planID != "" {
			return options, errors.New("compact requires --confirm and does not accept session/plan selection")
		}
	}
	return maintenanceOptions{config: cfg, action: action, sessionID: session.ID(*id), planID: *planID, confirm: *confirm}, nil
}
