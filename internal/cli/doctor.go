package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/vkviyu/go-self-evolving-agent/internal/app"
	"github.com/vkviyu/go-self-evolving-agent/internal/config"
)

func runDoctor(ctx context.Context, arguments []string, stdout, stderr io.Writer) (result error) {
	profile, profileSet, err := profileArgument(arguments)
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.String("profile", "", "configuration profile loaded from GSEA_HOME/<profile>.config.toml")
	flags.String("p", "", "configuration profile loaded from GSEA_HOME/<profile>.config.toml")
	dataDirectory := flags.String("data-dir", "", "override the local Agent data directory")
	workspaceDirectory := flags.String("workspace", "", "override the workspace directory")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("doctor does not accept positional arguments")
	}
	loadOptions := config.LoadOptions{}
	if profileSet {
		loadOptions.Profile = profile
	}
	resolved, err := config.Load(loadOptions)
	if err != nil {
		return err
	}
	if *dataDirectory != "" {
		resolved.DataDirectory = *dataDirectory
	}
	if *workspaceDirectory != "" {
		resolved.WorkspaceDirectory = *workspaceDirectory
	}
	application, err := app.Open(ctx, resolved)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, application.Close()) }()
	report, err := application.Doctor(ctx)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		return fmt.Errorf("write doctor report: %w", err)
	}
	return nil
}
