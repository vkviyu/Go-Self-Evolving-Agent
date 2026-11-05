package evolutiontools

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/approval"
	"github.com/vkviyu/go-self-evolving-agent/internal/strategy"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

type strategySpy struct {
	spec                                                        strategy.Spec
	report                                                      strategy.Report
	saves, loads, reports, promotions, comparisons, resolutions int
	err                                                         error
}

func (store *strategySpy) Save(_ context.Context, spec strategy.Spec) (string, error) {
	store.saves++
	store.spec = spec
	return "candidate", store.err
}

func (store *strategySpy) Load(_ context.Context, id string) (strategy.Spec, error) {
	store.loads++
	if id != "candidate" {
		return strategy.Spec{}, errors.New("unknown candidate")
	}
	return store.spec, store.err
}

func (store *strategySpy) Report(_ context.Context, id string) (strategy.Report, error) {
	store.reports++
	if id != "report" {
		return strategy.Report{}, errors.New("unknown report")
	}
	return store.report, store.err
}

func (store *strategySpy) Promote(_ context.Context, id string, confirmed bool) error {
	store.promotions++
	if id != "report" || !confirmed {
		return errors.New("invalid promotion")
	}
	return store.err
}

func strategyBindingsFixture() (*strategySpy, []tool.Binding) {
	store := &strategySpy{
		spec:   strategy.Spec{Version: 1, Revision: "v1", OwnerSession: "owner", MaxIterations: 2},
		report: strategy.Report{CandidateID: "candidate", SuiteID: "suite", BaselineConfigID: "baseline", Eligible: true},
	}
	resolve := func(ctx context.Context) (StrategyCaller, error) {
		store.resolutions++
		invocation, ok := tool.InvocationFromContext(ctx)
		if !ok {
			return StrategyCaller{}, errors.New("missing trusted invocation")
		}
		return StrategyCaller{
			OwnerSession: string(invocation.SessionID), BaselineConfigID: "baseline",
			Compare: func(_ context.Context, candidateID, suiteID string) (strategy.Report, string, error) {
				store.comparisons++
				if candidateID != "candidate" || suiteID != "suite" {
					return strategy.Report{}, "", errors.New("comparison identity changed")
				}
				return store.report, "report", store.err
			},
		}, nil
	}
	return store, StrategyBindings(store, resolve)
}

func TestStrategyAdaptersPreserveOutputsAndTrustedOwner(t *testing.T) {
	store, bindings := strategyBindingsFixture()
	pipeline := approvedPipeline(t, bindings)
	raw, err := invoke(pipeline, "strategy_propose", []byte(`{"version":1,"revision":"v1","instruction_suffix":"bounded"}`))
	if err != nil || store.spec.OwnerSession != "owner" {
		t.Fatalf("untrusted proposal: %+v %v", store.spec, err)
	}
	assertJSON(t, raw, []byte(`{"candidate_id":"candidate","state":"candidate"}`))
	raw, err = invoke(pipeline, "strategy_inspect", []byte(`{"candidate_id":"candidate"}`))
	if err != nil {
		t.Fatal(err)
	}
	assertJSON(t, raw, mustJSON(t, store.spec))
	raw, err = invoke(pipeline, "strategy_shadow", []byte(`{"candidate_id":"candidate","suite_id":"suite"}`))
	if err != nil || store.comparisons != 1 {
		t.Fatalf("comparison error = %v", err)
	}
	assertJSON(t, raw, mustJSON(t, map[string]any{"report_id": "report", "report": store.report}))
	raw, err = invoke(pipeline, "strategy_publish", []byte(`{"report_id":"report"}`))
	if err != nil || store.promotions != 1 {
		t.Fatalf("publication error = %v", err)
	}
	assertJSON(t, raw, []byte(`{"report_id":"report","available_next_turn":true}`))

	// A fresh invocation/pipeline prevents result replay from masking report inspection.
	raw, err = invoke(approvedPipeline(t, bindings), "strategy_inspect", []byte(`{"report_id":"report"}`))
	if err != nil {
		t.Fatal(err)
	}
	assertJSON(t, raw, mustJSON(t, store.report))
}

func TestStrategyScopeAndSelectionFailBeforeEffects(t *testing.T) {
	for _, tc := range []struct{ name, tool, raw, owner, baseline, want string }{
		{"no selection", "strategy_inspect", `{}`, "owner", "baseline", "candidate_id or report_id is required"},
		{"mixed candidate", "strategy_publish", `{"report_id":"report","candidate_id":"candidate"}`, "owner", "baseline", "report_id cannot be combined"},
		{"mixed suite", "strategy_publish", `{"report_id":"report","suite_id":"suite"}`, "owner", "baseline", "report_id cannot be combined"},
		{"foreign candidate", "strategy_inspect", `{"candidate_id":"candidate"}`, "foreign", "baseline", "different session"},
		{"foreign report", "strategy_publish", `{"report_id":"report"}`, "foreign", "baseline", "different session"},
		{"foreign baseline", "strategy_publish", `{"report_id":"report"}`, "owner", "foreign", "different baseline config"},
		{"missing report", "strategy_publish", `{"candidate_id":"candidate"}`, "owner", "baseline", "publish requires report_id only"},
		{"missing suite", "strategy_shadow", `{"candidate_id":"candidate"}`, "owner", "baseline", "shadow requires candidate_id and suite_id only"},
		{"shadow report", "strategy_shadow", `{"report_id":"report"}`, "owner", "baseline", "shadow requires candidate_id and suite_id only"},
		{"unknown candidate", "strategy_shadow", `{"candidate_id":"missing","suite_id":"suite"}`, "owner", "baseline", "unknown candidate"},
		{"unknown report", "strategy_inspect", `{"report_id":"missing"}`, "owner", "baseline", "unknown report"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, bindings := strategyBindingsFixture()
			store.spec.OwnerSession, store.report.BaselineConfigID = tc.owner, tc.baseline
			raw, err := invoke(approvedPipeline(t, bindings), tc.tool, []byte(tc.raw))
			if err == nil || !strings.Contains(err.Error(), tc.want) || len(raw) != 0 {
				t.Fatalf("output=%s error=%v", raw, err)
			}
			if store.saves+store.promotions+store.comparisons != 0 {
				t.Fatal("invalid request caused effects")
			}
		})
	}
}

func TestStrategySchemaApprovalAndCallerFailuresDoNotReachServices(t *testing.T) {
	for _, tc := range []struct{ name, raw string }{
		{"strategy_propose", `{"version":1,"revision":"v1","instruction_suffix":"x"}`},
		{"strategy_shadow", `{"candidate_id":"candidate","suite_id":"suite"}`},
		{"strategy_publish", `{"report_id":"report"}`},
	} {
		store, bindings := strategyBindingsFixture()
		if _, err := invoke(testPipeline(t, bindings, approval.DenyApprover{}), tc.name, []byte(tc.raw)); err == nil || store.resolutions != 0 {
			t.Fatal("denied request reached caller resolution")
		}
	}
	store, bindings := strategyBindingsFixture()
	if _, err := invoke(approvedPipeline(t, bindings), "strategy_propose", []byte(`{"version":1,"revision":"v1","instruction_suffix":"x","owner_session":"foreign"}`)); err == nil || store.saves != 0 {
		t.Fatal("model-supplied owner must be rejected before the service")
	}
	if _, err := invoke(approvedPipeline(t, bindings), "strategy_propose", []byte(`{"version":1,"revision":"v1","instruction_suffix":"x"}`)); err != nil || store.saves != 1 || store.spec.OwnerSession != "owner" {
		t.Fatal("valid request did not use the trusted invocation owner")
	}
	store, bindings = strategyBindingsFixture()
	for _, binding := range bindings {
		if _, err := binding.Executor.Invoke(context.Background(), []byte(`{"candidate_id":"candidate"}`)); err == nil {
			t.Fatal("unscoped request accepted")
		}
	}
	if store.saves+store.loads+store.reports+store.promotions+store.comparisons != 0 {
		t.Fatal("unscoped request accessed services")
	}
}

func TestStrategyAdapterPreservesFatalEvidenceError(t *testing.T) {
	store, bindings := strategyBindingsFixture()
	store.err = strategy.ErrEvidence
	_, err := invoke(approvedPipeline(t, bindings), "strategy_propose", []byte(`{"version":1,"revision":"v1","instruction_suffix":"x"}`))
	var repairable *tool.ExecutionError
	if !errors.Is(err, strategy.ErrEvidence) || errors.As(err, &repairable) {
		t.Fatalf("fatal error became repairable: %v", err)
	}
}
