package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/vkviyu/go-self-evolving-agent/internal/app"
)

func runMaintenance(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	options, err := parseMaintenanceOptions(args, stderr)
	if err != nil {
		return err
	}
	return executeMaintenance(ctx, options, stdout, stderr)
}

func executeMaintenance(ctx context.Context, options maintenanceOptions, stdout, stderr io.Writer) error {
	cfg, action := options.config, options.action
	var err error
	var output any
	switch action {
	case "plan":
		output, err = app.PreviewRetention(ctx, cfg, options.sessionID)
	case "archive":
		output, err = app.ArchiveSession(ctx, cfg, options.sessionID, options.planID, options.confirm)
		if err == nil {
			fmt.Fprintln(stderr, "Recovery archived; history, checkpoints and prior side effects were not deleted or undone.")
		}
	case "compact":
		err = app.CompactDatabase(ctx, cfg, options.confirm)
		output = map[string]any{"compacted": err == nil, "evidence_deleted": false}
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(output)
}
