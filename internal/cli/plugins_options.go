package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/vkviyu/go-self-evolving-agent/internal/config"
)

type pluginOptions struct {
	config       config.Config
	action       string
	name         string
	revision     string
	manifestPath string
	wasmPath     string
	casesPath    string
	requestPath  string
	buildID      string
	reportID     string
	planID       string
	confirm      bool
}

// parsePluginOptions loads read-only configuration and validates flags. It
// never reads candidate files, opens a database or constructs executable code.
func parsePluginOptions(args []string, stderr io.Writer) (options pluginOptions, err error) {
	if len(args) == 0 {
		return options, errors.New("plugins requires list, stage, build, inspect-build, test, activate, retire, rollback, gc-plan or collect")
	}
	action := args[0]
	switch action {
	case "list", "stage", "build", "inspect-build", "test", "activate", "retire", "rollback", "gc-plan", "collect":
	default:
		return options, errors.New("unknown plugins operation")
	}
	profile, _, err := profileArgument(args[1:])
	if err != nil {
		return options, err
	}
	cfg, err := config.Load(config.LoadOptions{Profile: profile})
	if err != nil {
		return options, err
	}
	flags := flag.NewFlagSet("plugins "+action, flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&profile, "profile", profile, "configuration profile")
	flags.StringVar(&profile, "p", profile, "configuration profile")
	flags.StringVar(&cfg.DataDirectory, "data-dir", cfg.DataDirectory, "owned database directory")
	flags.StringVar(&cfg.WorkspaceDirectory, "workspace", cfg.WorkspaceDirectory, "workspace identity for native bindings")
	name := flags.String("tool", "", "exact tool name")
	revision := flags.String("revision", "", "exact revision")
	manifestPath := flags.String("manifest", "", "trusted plugin Manifest JSON")
	wasmPath := flags.String("wasm", "", "compiled WASM artifact")
	casesPath := flags.String("cases", "", "JSON array of explicit behavior tests")
	requestPath := flags.String("request", "", "bounded candidate build Request JSON")
	buildID := flags.String("build-id", "", "immutable build evidence identity")
	reportID := flags.String("report-id", "", "reviewed passing test report ID")
	planID := flags.String("plan-id", "", "reviewed collection plan ID")
	confirm := flags.Bool("confirm", false, "explicitly approve publication, retirement, or irreversible artifact collection")
	if err := flags.Parse(args[1:]); err != nil {
		return options, err
	}
	if flags.NArg() != 0 {
		return options, errors.New("plugins accepts no positional arguments")
	}
	allowed := map[string]map[string]bool{
		"build": {"request": true}, "inspect-build": {"build-id": true},
		"list": {}, "stage": {"manifest": true, "wasm": true}, "test": {"tool": true, "revision": true, "cases": true},
		"activate": {"tool": true, "revision": true, "report-id": true, "confirm": true}, "rollback": {"tool": true, "revision": true, "report-id": true, "confirm": true},
		"retire": {"tool": true, "revision": true, "confirm": true}, "gc-plan": {"tool": true, "revision": true}, "collect": {"tool": true, "revision": true, "plan-id": true, "confirm": true},
	}
	var invalid string
	flags.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "data-dir", "workspace", "profile", "p":
			return
		}
		if !allowed[action][f.Name] {
			invalid = f.Name
		}
	})
	if invalid != "" {
		return options, fmt.Errorf("--%s is not valid for plugins %s", invalid, action)
	}
	if action != "list" && action != "stage" && action != "build" && action != "inspect-build" && (*name == "" || *revision == "") {
		return options, errors.New("operation requires exact --tool and --revision")
	}
	if (action == "activate" || action == "rollback") && (!*confirm || *reportID == "") {
		return options, errors.New("publication requires --report-id and --confirm")
	}
	if (action == "retire" || action == "collect") && !*confirm {
		return options, errors.New("operation requires --confirm")
	}
	if action == "collect" && *planID == "" {
		return options, errors.New("collection requires --plan-id")
	}
	if action == "build" && *requestPath == "" {
		return options, errors.New("build requires --request")
	}
	if action == "inspect-build" && *buildID == "" {
		return options, errors.New("inspect-build requires --build-id")
	}
	if action == "stage" && (*manifestPath == "" || *wasmPath == "") {
		return options, errors.New("stage requires --manifest and --wasm")
	}
	if action == "test" && *casesPath == "" {
		return options, errors.New("test requires --cases")
	}
	return pluginOptions{
		config:       cfg,
		action:       action,
		name:         *name,
		revision:     *revision,
		manifestPath: *manifestPath,
		wasmPath:     *wasmPath,
		casesPath:    *casesPath,
		requestPath:  *requestPath,
		buildID:      *buildID,
		reportID:     *reportID,
		planID:       *planID,
		confirm:      *confirm,
	}, nil
}
