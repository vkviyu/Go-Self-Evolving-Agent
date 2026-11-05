package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/vkviyu/go-self-evolving-agent/internal/app"
	"github.com/vkviyu/go-self-evolving-agent/internal/plugin"
	"github.com/vkviyu/go-self-evolving-agent/internal/pluginbuild"
)

func runPlugins(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	options, err := parsePluginOptions(args, stderr)
	if err != nil {
		return err
	}
	return executePlugins(ctx, options, stdout, stderr)
}

func executePlugins(ctx context.Context, options pluginOptions, stdout, stderr io.Writer) (result error) {
	cfg, action := options.config, options.action
	var err error
	var output any
	switch action {
	case "gc-plan":
		output, err = app.PreviewPluginCollection(ctx, cfg, options.name, options.revision)
	case "collect":
		output, err = app.CollectPlugin(ctx, cfg, options.name, options.revision, options.planID, options.confirm)
		if err == nil {
			fmt.Fprintln(stderr, "Plugin revision collected; its original identity and audit remain. Deleted WASM cannot be restored under that revision. Shared blobs remain retained. Run maintenance compact to return freed pages to disk.")
		}
	default:
		input, inputErr := loadPluginInput(options)
		if inputErr != nil {
			return inputErr
		}
		application, openErr := app.Open(ctx, cfg)
		if openErr != nil {
			return openErr
		}
		defer func() { result = errors.Join(result, application.Close()) }()
		switch action {
		case "build":
			var built pluginbuild.Result
			built, err = application.BuildPlugin(ctx, input.request)
			output = built
			if err != nil {
				if built.BuildID != "" {
					if writeErr := json.NewEncoder(stdout).Encode(built); writeErr != nil {
						return errors.Join(err, writeErr)
					}
				}
				return err
			}
			if built.State != "awaiting_approval" {
				if writeErr := json.NewEncoder(stdout).Encode(built); writeErr != nil {
					return writeErr
				}
				return errors.New("candidate is quarantined or failed; repair under a new revision")
			}
		case "inspect-build":
			var request pluginbuild.Request
			var built *pluginbuild.Result
			request, built, err = application.PluginBuild(ctx, options.buildID)
			output = map[string]any{"request": request, "result": built}
		case "list":
			output = application.Plugins().Inventory()
		case "stage":
			var artifact *plugin.Artifact
			artifact, err = application.Plugins().Stage(ctx, input.manifest, input.source)
			if err == nil {
				output = map[string]any{"manifest": artifact.Manifest(), "content_sha256": artifact.ContentSHA256(), "state": artifact.State()}
			}
		case "test":
			var report plugin.TestReport
			var id string
			report, id, err = application.VerifyPlugin(ctx, options.name, options.revision, input.cases)
			output = map[string]any{"report_id": id, "report": report}
			if err == nil && !report.Passed {
				if writeErr := json.NewEncoder(stdout).Encode(output); writeErr != nil {
					return writeErr
				}
				return errors.New("plugin behavior tests failed; revision was not activated")
			}
		case "activate", "rollback":
			_, err = application.PromotePlugin(ctx, options.name, options.revision, options.reportID, options.confirm)
			output = map[string]any{"tool": options.name, "revision": options.revision, "state": "active", "report_id": options.reportID}
		case "retire":
			_, err = application.Plugins().Retire(options.name, options.revision)
			output = map[string]any{"tool": options.name, "revision": options.revision, "state": "retired"}
		}
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(output)
}
