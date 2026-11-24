package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/vkviyu/go-self-evolving-agent/internal/app"
	"github.com/vkviyu/go-self-evolving-agent/internal/strategy"
)

// Only shadow constructs a model; every operation is flag-driven, not stdin.
func runStrategy(ctx context.Context, args []string, stdout, stderr io.Writer, deps dependencies) error {
	options, err := parseStrategyOptions(args, stderr)
	if err != nil {
		return err
	}
	return executeStrategy(ctx, options, stdout, deps)
}

func executeStrategy(ctx context.Context, options strategyOptions, stdout io.Writer, deps dependencies) (result error) {
	cfg, action := options.config, options.action
	a, err := app.Open(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, a.Close()) }()
	var output any
	switch action {
	case "propose":
		var spec strategy.Spec
		if err = readStrategyJSON(options.file, maxStrategySpecBytes, &spec); err == nil {
			var id string
			id, err = a.Strategies().Save(ctx, spec)
			output = map[string]any{"candidate_id": id, "spec": spec}
		}
	case "suite":
		var value strategy.Suite
		if err = readStrategyJSON(options.file, maxStrategySuiteBytes, &value); err == nil {
			var id string
			id, err = a.Strategies().SaveSuite(ctx, value, true)
			output = map[string]any{"suite_id": id}
		}
	case "inspect":
		output, err = a.Strategies().Load(ctx, options.candidateID)
	case "report":
		output, err = a.Strategies().Report(ctx, options.reportID)
	case "publish":
		err = a.Strategies().Promote(ctx, options.reportID, true)
		output = map[string]any{"report_id": options.reportID, "available_next_turn": true}
	case "rollback":
		err = a.Strategies().Fallback(ctx, options.baselineID, options.candidateID, "explicit host fallback; no effect rollback or replay")
		output = map[string]any{"baseline_id": options.baselineID, "candidate_id": options.candidateID, "fallback_requested": true}
	case "shadow":
		// Validate durable identities before even constructing a provider client.
		if _, err = a.Strategies().Load(ctx, options.candidateID); err != nil {
			return err
		}
		if _, err = a.Strategies().Suite(ctx, options.suiteID); err != nil {
			return err
		}
		if deps.newModel == nil {
			return errors.New("model builder is unavailable")
		}
		chatModel, err := deps.newModel(ctx, options.chat.model)
		if err != nil {
			return err
		}
		root, _, err := a.EnsureRootThread(ctx, options.chat.sessionID)
		if err != nil {
			return err
		}
		factory, err := a.NewAgentFactory(chatModel, rootRuntimeConfig(options.chat, root.ActorScope(), a.Orchestration()))
		if err != nil {
			return err
		}
		report, id, err := a.ShadowStrategy(ctx, factory, options.candidateID, options.suiteID)
		if err != nil {
			return err
		}
		output = map[string]any{"report_id": id, "report": report}
		if !report.Eligible {
			if err := json.NewEncoder(stdout).Encode(output); err != nil {
				return err
			}
			return errors.New("strategy failed the shadow gate; no promotion")
		}
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(output)
}
