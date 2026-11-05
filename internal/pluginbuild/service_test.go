package pluginbuild

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/plugin"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

// A synchronous recorder makes the reserve-before-dispatch contract explicit
// without requiring a database, SDK installation or executable WASM guest.
type buildRecorder struct {
	steps       []string
	request     Request
	source      string
	manifest    plugin.Manifest
	artifact    []byte
	toolName    string
	revision    string
	cases       []plugin.TestCase
	saved       Result
	completed   *Result
	failAt      string
	failure     error
	finishErr   error
	passed      bool
	compileHook func(context.Context)
	finishHook  func(context.Context)
}

func (r *buildRecorder) step(name string) error {
	r.steps = append(r.steps, name)
	if r.failAt == name {
		return r.failure
	}
	return nil
}

func (r *buildRecorder) BeginPluginBuild(_ context.Context, request Request) (string, *Result, error) {
	r.request = request
	return "build-id", r.completed, r.step("reserve")
}

func (r *buildRecorder) FinishPluginBuild(ctx context.Context, result Result) error {
	r.saved = result
	_ = r.step("finish")
	if r.finishHook != nil {
		r.finishHook(ctx)
	}
	return r.finishErr
}

func (r *buildRecorder) PluginBuild(_ context.Context, id string) (Request, *Result, error) {
	if id != "build-id" {
		return Request{}, nil, errors.New("unexpected build ID")
	}
	return r.request, &r.saved, r.step("inspect")
}

func (r *buildRecorder) Compile(ctx context.Context, source string) ([]byte, BuildEvidence, error) {
	r.source = source
	if r.compileHook != nil {
		r.compileHook(ctx)
	}
	err := r.step("compile")
	return []byte("wasm"), BuildEvidence{SourceSHA256: "source-sha", Success: err == nil}, err
}

func (r *buildRecorder) Stage(_ context.Context, manifest plugin.Manifest, artifact []byte) (*plugin.Artifact, error) {
	r.manifest, r.artifact = manifest, artifact
	return nil, r.step("stage")
}

func (r *buildRecorder) Verify(_ context.Context, name, revision string, cases []plugin.TestCase) (plugin.TestReport, string, error) {
	r.toolName, r.revision, r.cases = name, revision, cases
	return plugin.TestReport{Passed: r.passed}, "report-id", r.step("verify")
}

func newRecordedBuild(t *testing.T, recorder *buildRecorder) *Service {
	t.Helper()
	service, err := NewService(ServiceConfig{Store: recorder, Compiler: recorder, Stager: recorder, Verifier: recorder})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func buildRequest() Request {
	return Request{
		OwnerSession: "owner",
		Manifest: plugin.Manifest{PluginID: "echo", Tool: tool.Descriptor{
			Name: "extension_echo", Revision: "v1", Description: "Echo JSON",
			RiskClass: tool.RiskPure, InputSchema: []byte(`{"type":"object"}`),
		}},
		Source: echoSource,
		Cases:  []plugin.TestCase{{Name: "echo", Input: []byte(`{}`), Expected: []byte(`{}`)}},
	}
}

func TestBuildServiceWorkflow(t *testing.T) {
	failure := errors.New("injected failure")
	for _, tc := range []struct {
		name, failAt, state string
		passed              bool
		steps               []string
	}{
		{"passing", "", "awaiting_approval", true, []string{"reserve", "compile", "stage", "verify", "finish"}},
		{"failing cases", "", "quarantined", false, []string{"reserve", "compile", "stage", "verify", "finish"}},
		{"reservation error", "reserve", "", false, []string{"reserve"}},
		{"compiler error", "compile", "failed", false, []string{"reserve", "compile", "finish"}},
		{"staging error", "stage", "failed", false, []string{"reserve", "compile", "stage", "finish"}},
		{"verification error", "verify", "failed", false, []string{"reserve", "compile", "stage", "verify", "finish"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := &buildRecorder{failAt: tc.failAt, failure: failure, passed: tc.passed}
			service := newRecordedBuild(t, recorder)
			result, err := service.Build(context.Background(), buildRequest())
			if (tc.failAt == "" && err != nil) || (tc.failAt != "" && !errors.Is(err, failure)) {
				t.Fatalf("error = %v", err)
			}
			if result.State != tc.state || !reflect.DeepEqual(recorder.steps, tc.steps) {
				t.Fatalf("state=%q steps=%v", result.State, recorder.steps)
			}
			if recorder.request.Manifest.Timeout != 5*time.Second {
				t.Fatalf("unbounded default timeout: %v", recorder.request.Manifest.Timeout)
			}
			if tc.failAt == "reserve" {
				if result.BuildID != "" {
					t.Fatal("claimed receipt before reservation succeeded")
				}
				return
			}
			if result.BuildID != "build-id" || !reflect.DeepEqual(result, recorder.saved) || result.Build.SourceSHA256 != "source-sha" {
				t.Fatalf("missing or changed evidence: result=%+v saved=%+v", result, recorder.saved)
			}
			if tc.failAt != "" && result.Error != failure.Error() {
				t.Fatalf("diagnostic = %q", result.Error)
			}
			if tc.failAt == "" {
				if r := recorder; r.source != r.request.Source || !reflect.DeepEqual(r.manifest, r.request.Manifest) || string(r.artifact) != "wasm" || r.toolName != r.request.Manifest.Tool.Name || r.revision != r.request.Manifest.Tool.Revision || !reflect.DeepEqual(r.cases, r.request.Cases) {
					t.Fatal("workflow changed the reserved candidate, compiled bytes or test cases")
				}
				if result.TestReportID != "report-id" || result.TestReport == nil || result.TestReport.Passed != tc.passed {
					t.Fatal("workflow lost the verification report")
				}
			}
			request, inspected, err := service.Inspect(context.Background(), result.BuildID)
			if err != nil || !reflect.DeepEqual(request, recorder.request) || !reflect.DeepEqual(*inspected, result) {
				t.Fatalf("inspection lost evidence: %v", err)
			}
		})
	}
}

func TestBuildServiceReplaysReceiptWithoutDispatch(t *testing.T) {
	completed := Result{BuildID: "original", State: "quarantined", TestReportID: "old-report"}
	recorder := &buildRecorder{completed: &completed}
	result, err := newRecordedBuild(t, recorder).Build(context.Background(), buildRequest())
	if err != nil || !reflect.DeepEqual(result, completed) || !reflect.DeepEqual(recorder.steps, []string{"reserve"}) {
		t.Fatalf("receipt rerun: result=%+v steps=%v err=%v", result, recorder.steps, err)
	}
}

func TestBuildServiceRejectsInvalidRequestBeforeReservation(t *testing.T) {
	recorder := &buildRecorder{}
	request := buildRequest()
	request.Manifest.Tool.RiskClass = tool.RiskWrite
	if _, err := newRecordedBuild(t, recorder).Build(context.Background(), request); err == nil || len(recorder.steps) != 0 {
		t.Fatalf("unsafe request dispatched: steps=%v err=%v", recorder.steps, err)
	}
}

func TestBuildServicePreservesEvidenceAfterCancellation(t *testing.T) {
	type contextKey struct{}
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), contextKey{}, "trace"))
	defer cancel()
	recorder := &buildRecorder{
		failAt: "compile", failure: context.Canceled,
		compileHook: func(context.Context) { cancel() },
		finishHook: func(recordCtx context.Context) {
			deadline, ok := recordCtx.Deadline()
			if recordCtx.Err() != nil || !ok || time.Until(deadline) > 5*time.Second || recordCtx.Value(contextKey{}) != "trace" {
				t.Error("evidence context lost values, inherited cancellation or has no bounded deadline")
			}
		},
	}
	result, err := newRecordedBuild(t, recorder).Build(ctx, buildRequest())
	if !errors.Is(err, context.Canceled) || result.State != "failed" || recorder.saved.Error != context.Canceled.Error() {
		t.Fatalf("canceled evidence: %+v %v", recorder.saved, err)
	}
}

func TestBuildServiceEvidenceFailureIsFatal(t *testing.T) {
	diskErr := errors.New("disk unavailable")
	compileErr := errors.New(strings.Repeat("x", 5000))
	recorder := &buildRecorder{failAt: "compile", failure: compileErr, finishErr: diskErr}
	result, err := newRecordedBuild(t, recorder).Build(context.Background(), buildRequest())
	var fatal interface{ ToolExecutionFatal() bool }
	if !errors.Is(err, diskErr) || !errors.Is(err, compileErr) || !errors.Is(err, ErrEvidence) || !errors.As(err, &fatal) || !fatal.ToolExecutionFatal() {
		t.Fatalf("lost fatal evidence failure: %v", err)
	}
	if len(result.Error) != 4096 || !reflect.DeepEqual(result, recorder.saved) {
		t.Fatalf("unbounded or changed diagnostic: %d", len(result.Error))
	}
}

func TestBuildServiceCanceledWaiterDoesNotReserve(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(release) })
	recorder := &buildRecorder{passed: true, compileHook: func(context.Context) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
		}
	}}
	service := newRecordedBuild(t, recorder)
	done := make(chan error, 1)
	go func() { _, err := service.Build(ctx, buildRequest()); done <- err }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("compiler did not start")
	}
	waiter, stop := context.WithCancel(ctx)
	stop()
	if _, err := service.Build(waiter, buildRequest()); !errors.Is(err, context.Canceled) {
		t.Fatalf("waiter error = %v", err)
	}
	// Cancel the blocked compiler and join before inspecting the recorder.
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(recorder.steps, []string{"reserve", "compile", "stage", "verify", "finish"}) {
		t.Fatalf("waiter dispatched work: %v", recorder.steps)
	}
}

func TestBuildServiceRequiresEveryDependency(t *testing.T) {
	recorder := &buildRecorder{}
	for _, missing := range []string{"store", "compiler", "stager", "verifier"} {
		t.Run(missing, func(t *testing.T) {
			config := ServiceConfig{Store: recorder, Compiler: recorder, Stager: recorder, Verifier: recorder}
			switch missing {
			case "store":
				config.Store = nil
			case "compiler":
				config.Compiler = nil
			case "stager":
				config.Stager = nil
			case "verifier":
				config.Verifier = nil
			}
			if _, err := NewService(config); err == nil {
				t.Fatal("accepted missing dependency")
			}
		})
	}
}
