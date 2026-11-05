package agentruntime

import (
	"context"
	"fmt"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentgoal"
	"github.com/vkviyu/go-self-evolving-agent/internal/policy"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/memorystore"
	"github.com/vkviyu/go-self-evolving-agent/internal/strategy"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

var _ strategy.Comparator = (*Factory)(nil)

// CompareStrategy runs two isolated Eino sessions per host-owned fixture. No
// executable business tools, approval tools, plugin proposal or publication
// tools exist in these registries. The model gateway/usage ledger stays shared.
func (f *Factory) CompareStrategy(ctx context.Context, candidate strategy.Spec, suite strategy.Suite) (strategy.Report, error) {
	report := strategy.Report{CandidateID: candidate.ID(), SuiteID: strategy.Digest(suite), BaselineConfigID: f.BaselineConfigID(), EvaluatedAt: time.Now().UTC()}
	if err := candidate.Validate(); err != nil {
		return report, err
	}
	if err := suite.Validate(); err != nil {
		return report, err
	}
	base, err := f.withActiveStrategy(ctx)
	if err != nil {
		return report, err
	}
	report.PreviousSpecID = base.strategyID
	changed, err := f.withStrategy(candidate)
	if err != nil {
		return report, err
	}
	shadowCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	for _, fixture := range suite.Fixtures {
		report.Baseline = append(report.Baseline, base.shadowFixture(shadowCtx, fixture))
		report.Candidate = append(report.Candidate, changed.shadowFixture(shadowCtx, fixture))
	}
	report.Eligible, report.Reason = report.Acceptance()
	return report, nil
}

func (f *Factory) shadowFixture(ctx context.Context, fixture strategy.Fixture) strategy.Outcome {
	outcome := strategy.Outcome{Fixture: fixture.Name, Safety: fixture.Safety}
	started := time.Now()
	run := func() error {
		events := memorystore.New()
		registry := tool.NewRegistry()
		engine, err := policy.DefaultDeny()
		if err != nil {
			return err
		}
		pipeline, err := tool.NewPipeline(tool.PipelineConfig{Registry: registry, Policy: engine, Events: events})
		if err != nil {
			return err
		}
		cfg := f.config
		cfg.GoalMode = agentgoal.Config{}
		cfg.GoalStore = nil
		cfg.GoalEvaluator = nil
		cfg.ContextArtifacts = nil
		cfg.Strategies = nil
		factory, err := NewFactory(cfg, f.model, registry, pipeline, events, events)
		if err != nil {
			return err
		}
		sessionID, err := randomID()
		if err != nil {
			return err
		}
		caseCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		turn, err := factory.NewTurn(caseCtx, session.ID("shadow-"+sessionID))
		if err != nil {
			return err
		}
		result, runErr := turn.Run(caseCtx, []*schema.Message{schema.UserMessage(fixture.Prompt)}, nil)
		items, err := events.Load(ctx, session.ID("shadow-"+sessionID), 0, 10000)
		if err != nil {
			return err
		}
		for _, event := range items {
			if event.Type == session.EventModelRequested {
				outcome.ModelCalls++
			}
		}
		if runErr != nil {
			return runErr
		}
		if result.Final != nil {
			outcome.OutputBytes = len(result.Final.Content)
			outcome.Passed = result.Final.Content == fixture.Expected
		}
		return nil
	}
	if err := run(); err != nil {
		outcome.Error = fmt.Sprintf("%.512s", err.Error())
	}
	outcome.DurationMS = time.Since(started).Milliseconds()
	return outcome
}
