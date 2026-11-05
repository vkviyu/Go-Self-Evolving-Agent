package strategy

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

type shadowRecorder struct {
	steps    []string
	failAt   string
	failure  error
	spec     Spec
	suite    Suite
	saved    Report
	saveHook func(context.Context)
}

func (r *shadowRecorder) step(name string) error {
	r.steps = append(r.steps, name)
	if r.failAt == name {
		return r.failure
	}
	return nil
}

func (r *shadowRecorder) Load(_ context.Context, id string) (Spec, error) {
	if id != r.spec.ID() {
		return Spec{}, errors.New("unexpected candidate ID")
	}
	return r.spec, r.step("load")
}

func (r *shadowRecorder) Suite(_ context.Context, id string) (Suite, error) {
	if id != Digest(r.suite) {
		return Suite{}, errors.New("unexpected suite ID")
	}
	return r.suite, r.step("suite")
}

func (r *shadowRecorder) SaveReport(ctx context.Context, report Report) (string, error) {
	r.saved = report
	if r.saveHook != nil {
		r.saveHook(ctx)
	}
	return "report-id", r.step("save")
}

type comparatorFunc func(context.Context, Spec, Suite) (Report, error)

func (compare comparatorFunc) CompareStrategy(ctx context.Context, spec Spec, suite Suite) (Report, error) {
	return compare(ctx, spec, suite)
}

func shadowFixture(t *testing.T) (*ShadowService, *shadowRecorder) {
	t.Helper()
	recorder := &shadowRecorder{
		spec: Spec{Version: 1, Revision: "v1", OwnerSession: "owner", MaxIterations: 2},
		suite: Suite{Name: "trusted", Fixtures: []Fixture{
			{Name: "safety", Prompt: "host prompt", Expected: "host expectation", Safety: true},
		}},
	}
	service, err := NewShadowService(recorder)
	if err != nil {
		t.Fatal(err)
	}
	return service, recorder
}

func TestShadowServiceWorkflow(t *testing.T) {
	failure := errors.New("injected failure")
	for _, tc := range []struct {
		name, failAt string
		steps        []string
	}{
		{"success", "", []string{"load", "suite", "compare", "save"}},
		{"candidate error", "load", []string{"load"}},
		{"suite error", "suite", []string{"load", "suite"}},
		{"compare error", "compare", []string{"load", "suite", "compare", "save"}},
		{"evidence error", "save", []string{"load", "suite", "compare", "save"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service, recorder := shadowFixture(t)
			recorder.failAt, recorder.failure = tc.failAt, failure
			expected := Report{CandidateID: recorder.spec.ID(), SuiteID: Digest(recorder.suite), Reason: "fixture report"}
			compare := comparatorFunc(func(_ context.Context, spec Spec, suite Suite) (Report, error) {
				if !reflect.DeepEqual(spec, recorder.spec) || !reflect.DeepEqual(suite, recorder.suite) {
					t.Fatal("comparison did not use the stored candidate and trusted expectations")
				}
				return expected, recorder.step("compare")
			})
			report, id, err := service.Compare(context.Background(), compare, recorder.spec.ID(), Digest(recorder.suite))
			if (tc.failAt == "" && err != nil) || (tc.failAt != "" && !errors.Is(err, failure)) {
				t.Fatalf("error = %v", err)
			}
			if !reflect.DeepEqual(recorder.steps, tc.steps) {
				t.Fatalf("steps = %v", recorder.steps)
			}
			if tc.failAt == "save" {
				var fatal interface{ ToolExecutionFatal() bool }
				if !errors.Is(err, ErrEvidence) || !errors.As(err, &fatal) || !fatal.ToolExecutionFatal() {
					t.Fatalf("evidence failure is not fatal: %v", err)
				}
			} else if errors.Is(err, ErrEvidence) {
				t.Fatalf("ordinary failure misclassified: %v", err)
			}
			if tc.failAt == "compare" {
				if id != "report-id" || report.Failure == "" || report.Eligible || !reflect.DeepEqual(recorder.saved, report) {
					t.Fatalf("failed comparison evidence lost: report=%+v saved=%+v id=%s", report, recorder.saved, id)
				}
			} else if tc.failAt == "" || tc.failAt == "save" {
				if id != "report-id" || !reflect.DeepEqual(report, expected) || !reflect.DeepEqual(recorder.saved, expected) {
					t.Fatalf("changed report: report=%+v saved=%+v id=%s", report, recorder.saved, id)
				}
			} else if id != "" {
				t.Fatal("claimed durable report before saving")
			}
		})
	}
}

func TestShadowServiceSavesCanceledOutcomes(t *testing.T) {
	service, recorder := shadowFixture(t)
	type contextKey struct{}
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), contextKey{}, "trace"))
	defer cancel()
	recorder.saveHook = func(recordCtx context.Context) {
		deadline, ok := recordCtx.Deadline()
		if recordCtx.Err() != nil || !ok || time.Until(deadline) > 5*time.Second || recordCtx.Value(contextKey{}) != "trace" {
			t.Error("evidence context lost values, inherited cancellation or has no bounded deadline")
		}
	}
	compare := comparatorFunc(func(context.Context, Spec, Suite) (Report, error) {
		cancel()
		return Report{Reason: "canceled", Candidate: []Outcome{{Fixture: "safety", Error: context.Canceled.Error()}}}, nil
	})
	report, id, err := service.Compare(ctx, compare, recorder.spec.ID(), Digest(recorder.suite))
	if err != nil || id != "report-id" || report.Eligible || !reflect.DeepEqual(recorder.saved, report) {
		t.Fatalf("canceled outcome lost: report=%+v id=%s err=%v", report, id, err)
	}
}

func TestShadowServiceCanceledWaiterDoesNotDispatch(t *testing.T) {
	service, recorder := shadowFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entered := make(chan struct{})
	compare := comparatorFunc(func(context.Context, Spec, Suite) (Report, error) {
		close(entered)
		<-ctx.Done()
		return Report{Reason: "canceled"}, nil
	})
	done := make(chan error, 1)
	go func() {
		_, _, err := service.Compare(ctx, compare, recorder.spec.ID(), Digest(recorder.suite))
		done <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("comparison did not start")
	}
	waiter, stop := context.WithCancel(ctx)
	stop()
	if _, _, err := service.Compare(waiter, compare, recorder.spec.ID(), Digest(recorder.suite)); !errors.Is(err, context.Canceled) {
		t.Fatalf("waiter error = %v", err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(recorder.steps, []string{"load", "suite", "save"}) {
		t.Fatalf("waiter dispatched work: %v", recorder.steps)
	}
}

func TestShadowServiceMissingDependencies(t *testing.T) {
	if _, err := NewShadowService(nil); err == nil {
		t.Fatal("accepted missing store")
	}
	service, recorder := shadowFixture(t)
	if _, _, err := service.Compare(context.Background(), nil, recorder.spec.ID(), Digest(recorder.suite)); err == nil || len(recorder.steps) != 0 {
		t.Fatalf("missing comparator dispatched: steps=%v err=%v", recorder.steps, err)
	}
}
