package cli

import (
	"errors"
	"flag"
	"io"

	"github.com/vkviyu/go-self-evolving-agent/internal/config"
)

type strategyOptions struct {
	config      config.Config
	action      string
	file        string
	candidateID string
	suiteID     string
	reportID    string
	baselineID  string
	chat        chatOptions
}

// parseStrategyOptions is read-only. Only shadow accepts model options, and
// confirmation is validated before any runtime or model can be constructed.
func parseStrategyOptions(args []string, stderr io.Writer) (parsed strategyOptions, err error) {
	if len(args) == 0 {
		return parsed, errors.New("strategy requires propose, suite, inspect, report, shadow, publish or rollback")
	}
	action := args[0]
	allowed := map[string][]string{
		"propose": {"file"}, "suite": {"file", "confirm"}, "inspect": {"candidate-id"}, "report": {"report-id"},
		"shadow": {"candidate-id", "suite-id", "confirm"}, "publish": {"report-id", "confirm"}, "rollback": {"baseline-id", "candidate-id", "confirm"},
	}
	selected, ok := allowed[action]
	if !ok {
		return parsed, errors.New("unknown strategy operation")
	}
	flags := flag.NewFlagSet("strategy "+action, flag.ContinueOnError)
	flags.SetOutput(stderr)
	profile := flags.String("profile", "", "configuration profile")
	data := flags.String("data-dir", "", "owned database directory")
	workspace := flags.String("workspace", "", "workspace directory")
	file := flags.String("file", "", "bounded spec or trusted suite JSON file")
	candidate := flags.String("candidate-id", "", "immutable LoopSpec ID")
	suite := flags.String("suite-id", "", "host-registered suite ID")
	reportID := flags.String("report-id", "", "exact shadow report ID")
	baseline := flags.String("baseline-id", "", "exact baseline config ID")
	confirm := flags.Bool("confirm", false, "confirm registration, paid shadow requests, promotion or fallback")
	if err := flags.Parse(args[1:]); err != nil {
		return parsed, err
	}
	valid := map[string]bool{"profile": true, "data-dir": true, "workspace": true}
	for _, name := range selected {
		valid[name] = true
	}
	var invalid bool
	flags.Visit(func(f *flag.Flag) {
		if !valid[f.Name] {
			invalid = true
		}
	})
	if invalid || (flags.NArg() > 0 && action != "shadow") {
		return parsed, errors.New("flags do not match strategy operation")
	}
	if (action == "suite" || action == "shadow" || action == "publish" || action == "rollback") && !*confirm {
		return parsed, errors.New("operation requires --confirm")
	}
	if (action == "propose" || action == "suite") && *file == "" {
		return parsed, errors.New("operation requires --file")
	}
	if (action == "inspect" || action == "shadow" || action == "rollback") && *candidate == "" {
		return parsed, errors.New("operation requires --candidate-id")
	}
	if (action == "report" || action == "publish") && *reportID == "" {
		return parsed, errors.New("operation requires --report-id")
	}
	if action == "rollback" && *baseline == "" {
		return parsed, errors.New("rollback requires --baseline-id")
	}
	if action == "shadow" && *suite == "" {
		return parsed, errors.New("shadow requires --suite-id")
	}
	load := config.LoadOptions{}
	if *profile != "" {
		load.Profile = *profile
	}
	cfg, err := config.Load(load)
	if err != nil {
		return parsed, err
	}
	if *data != "" {
		cfg.DataDirectory = *data
	}
	if *workspace != "" {
		cfg.WorkspaceDirectory = *workspace
	}
	var options chatOptions
	if action == "shadow" {
		if _, present, err := profileArgument(flags.Args()); err != nil {
			return parsed, err
		} else if present {
			return parsed, errors.New("put --profile before the -- separator so its defaults are loaded")
		}
		options, err = parseChatOptions(flags.Args(), cfg, stderr)
		if err != nil {
			return parsed, err
		}
		if options.evaluationMode || options.promptFile != "" || options.resumeTurnID != "" || options.listRecoveries || options.abandonRequest != nil || options.toolBridgeManifest != "" {
			return parsed, errors.New("shadow accepts model/config flags, not chat/recovery/evaluation operations")
		}
		cfg.DataDirectory = options.dataDirectory
		cfg.WorkspaceDirectory = options.workspaceDirectory
		cfg.Model.Name = options.model.Model
		cfg.Agent.Instruction = options.instruction
		cfg.Agent.MaxIterations = options.maxIterations
	}
	return strategyOptions{
		config:      cfg,
		action:      action,
		file:        *file,
		candidateID: *candidate,
		suiteID:     *suite,
		reportID:    *reportID,
		baselineID:  *baseline,
		chat:        options,
	}, nil
}
