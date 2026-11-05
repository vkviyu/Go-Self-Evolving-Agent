package evolutiontools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/approval"
	"github.com/vkviyu/go-self-evolving-agent/internal/pluginbuild"
	"github.com/vkviyu/go-self-evolving-agent/internal/policy"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/memorystore"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

func TestBindingsMatchPreRefactorProtocol(t *testing.T) {
	raw, err := os.ReadFile("testdata/bindings.json")
	if err != nil {
		t.Fatal(err)
	}
	var expected []tool.Descriptor
	if err := json.Unmarshal(raw, &expected); err != nil {
		t.Fatal(err)
	}
	bindings := append(PluginBindings(nil), StrategyBindings(nil, nil)...)
	actual := make([]tool.Descriptor, 0, len(bindings))
	for _, binding := range bindings {
		if err := binding.Descriptor.Validate(); err != nil || binding.Executor == nil {
			t.Fatalf("invalid binding %s: %v", binding.Descriptor.Name, err)
		}
		actual = append(actual, binding.Descriptor)
	}
	// Compare decoded JSON so indentation is not part of the contract. Names,
	// descriptions, schema, revision, risk, capabilities and ordering all are.
	assertJSON(t, mustJSON(t, actual), mustJSON(t, expected))
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func assertJSON(t *testing.T, actual, expected []byte) {
	t.Helper()
	var a, b any
	if err := json.Unmarshal(actual, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(expected, &b); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("JSON changed:\nactual: %s\nexpected: %s", actual, expected)
	}
}

func testPipeline(t *testing.T, bindings []tool.Binding, approver policy.Approver) *tool.Pipeline {
	t.Helper()
	registry := tool.NewRegistry()
	if _, err := registry.ActivateMany(bindings...); err != nil {
		t.Fatal(err)
	}
	engine, err := policy.DefaultDeny(policy.Rule{ID: "test-approval", Effect: policy.EffectRequireApproval})
	if err != nil {
		t.Fatal(err)
	}
	pipeline, err := tool.NewPipeline(tool.PipelineConfig{Registry: registry, Policy: engine, Approver: approver, Events: memorystore.New()})
	if err != nil {
		t.Fatal(err)
	}
	return pipeline
}

func approvedPipeline(t *testing.T, bindings []tool.Binding) *tool.Pipeline {
	t.Helper()
	names := make([]string, 0, len(bindings))
	for _, binding := range bindings {
		names = append(names, binding.Descriptor.Name)
	}
	return testPipeline(t, bindings, approval.NewToolAllowlistApprover(names...))
}

func invoke(pipeline *tool.Pipeline, name string, raw json.RawMessage) (json.RawMessage, error) {
	result, err := pipeline.Execute(context.Background(), tool.Call{
		SessionID: "owner", TurnID: "turn", CallID: name, ToolName: name, Arguments: raw,
	})
	return result.Data, err
}

func TestStrictEvolutionArguments(t *testing.T) {
	for _, raw := range []string{
		`{"build_id":"id","unknown":true}`, `{"build_id":"id"} {}`, `{"build_id":`, `[]`,
		string(make([]byte, pluginbuild.MaxRequestBytes+1)),
	} {
		var input struct {
			BuildID string `json:"build_id"`
		}
		if err := decodeEvolutionArgs([]byte(raw), &input); err == nil {
			t.Fatalf("accepted invalid JSON: %.80s", raw)
		}
	}
	var input struct {
		BuildID string `json:"build_id"`
	}
	if err := decodeEvolutionArgs([]byte(`{"build_id":"id"}`), &input); err != nil || input.BuildID != "id" {
		t.Fatalf("valid input rejected: %v", err)
	}
}

type pluginHostSpy struct {
	request                   pluginbuild.Request
	result                    *pluginbuild.Result
	err                       error
	builds, reads, promotions int
	published                 []any
}

func (host *pluginHostSpy) BuildPlugin(_ context.Context, request pluginbuild.Request) (pluginbuild.Result, error) {
	host.builds++
	host.request = request
	if host.result == nil {
		return pluginbuild.Result{}, host.err
	}
	return *host.result, host.err
}

func (host *pluginHostSpy) PluginBuild(_ context.Context, id string) (pluginbuild.Request, *pluginbuild.Result, error) {
	host.reads++
	if id != "build-id" {
		return pluginbuild.Request{}, nil, errors.New("unexpected build ID")
	}
	return host.request, host.result, host.err
}

func (host *pluginHostSpy) PromotePlugin(_ context.Context, name, revision, report string, confirmed bool) (*tool.Snapshot, error) {
	host.promotions++
	host.published = []any{name, revision, report, confirmed}
	return nil, host.err
}

func TestPluginProposalUsesInvocationAndPreservesErrorClassification(t *testing.T) {
	compileErr := errors.New("compiler rejected source")
	for _, tc := range []struct {
		name           string
		result         *pluginbuild.Result
		err            error
		wantErr, fatal bool
	}{
		{"passed", &pluginbuild.Result{BuildID: "build-id", State: "awaiting_approval"}, nil, false, false},
		{"repairable", &pluginbuild.Result{BuildID: "build-id", State: "failed", Error: compileErr.Error()}, compileErr, false, false},
		{"no reservation", nil, compileErr, true, false},
		{"uncertain evidence", &pluginbuild.Result{BuildID: "build-id"}, pluginbuild.ErrEvidence, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := &pluginHostSpy{result: tc.result, err: tc.err}
			pipeline := approvedPipeline(t, PluginBindings(host))
			raw, err := invoke(pipeline, "plugin_propose", []byte(`{"manifest":{},"source":"source","cases":[]}`))
			if (err != nil) != tc.wantErr || host.request.OwnerSession != "owner" || host.builds != 1 {
				t.Fatalf("scope/result changed: request=%+v calls=%d err=%v", host.request, host.builds, err)
			}
			if !tc.wantErr {
				assertJSON(t, raw, mustJSON(t, tc.result))
			} else {
				var repairable *tool.ExecutionError
				if !errors.Is(err, tc.err) || errors.As(err, &repairable) == tc.fatal {
					t.Fatalf("error classification changed: %v", err)
				}
			}
		})
	}
}

func TestPluginScopeAndPublicationGate(t *testing.T) {
	for _, tc := range []struct {
		name, owner, state, report string
		hasResult                  bool
		wantErr                    bool
	}{
		{"foreign", "foreign", "awaiting_approval", "report", true, true},
		{"interrupted", "owner", "", "", false, true},
		{"quarantined", "owner", "quarantined", "report", true, true},
		{"missing report", "owner", "awaiting_approval", "", true, true},
		{"passing", "owner", "awaiting_approval", "report", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := &pluginHostSpy{request: pluginbuild.Request{OwnerSession: tc.owner}}
			host.request.Manifest.Tool.Name, host.request.Manifest.Tool.Revision = "extension_test", "v1"
			if tc.hasResult {
				host.result = &pluginbuild.Result{State: tc.state, TestReportID: tc.report}
			}
			pipeline := approvedPipeline(t, PluginBindings(host))
			raw, err := invoke(pipeline, "plugin_publish", []byte(`{"build_id":"build-id"}`))
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v", err)
			}
			if tc.wantErr {
				if host.promotions != 0 {
					t.Fatal("invalid candidate published")
				}
			} else {
				if !reflect.DeepEqual(host.published, []any{"extension_test", "v1", "report", true}) {
					t.Fatal("publication identity changed")
				}
				assertJSON(t, raw, []byte(`{"tool":"extension_test","revision":"v1","available_next_turn":true}`))
			}
			raw, err = invoke(pipeline, "plugin_inspect", []byte(`{"build_id":"build-id"}`))
			if tc.owner != "owner" {
				if err == nil {
					t.Fatal("foreign evidence leaked")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				assertJSON(t, raw, mustJSON(t, map[string]any{"request": host.request, "result": host.result}))
			}
		})
	}
}

func TestPluginArgumentsAndApprovalCannotGrantScope(t *testing.T) {
	for _, tc := range []struct{ name, raw string }{
		{"plugin_propose", `{"manifest":{},"source":"x","cases":[]}`},
		{"plugin_publish", `{"build_id":"build-id"}`},
	} {
		host := &pluginHostSpy{}
		if _, err := invoke(testPipeline(t, PluginBindings(host), approval.DenyApprover{}), tc.name, []byte(tc.raw)); err == nil {
			t.Fatal("approval bypassed")
		}
		if host.builds+host.reads+host.promotions != 0 {
			t.Fatal("denied call reached service")
		}
	}
	host := &pluginHostSpy{}
	pipeline := approvedPipeline(t, PluginBindings(host))
	if _, err := invoke(pipeline, "plugin_propose", []byte(`{"manifest":{},"source":"x","cases":[],"owner_session":"foreign"}`)); err == nil || host.builds != 0 {
		t.Fatal("model-supplied owner must be rejected before the service")
	}
	// A repaired model call has a new durable call ID. Reusing the rejected
	// call's ID with changed arguments correctly produces an identity conflict.
	if _, err := pipeline.Execute(context.Background(), tool.Call{
		SessionID: "owner", TurnID: "turn", CallID: "repaired-proposal", ToolName: "plugin_propose",
		Arguments: json.RawMessage(`{"manifest":{},"source":"x","cases":[]}`),
	}); err != nil || host.builds != 1 || host.request.OwnerSession != "owner" {
		t.Fatalf("valid request did not use the trusted invocation owner: %v", err)
	}
	for _, binding := range PluginBindings(host) {
		if _, err := binding.Executor.Invoke(context.Background(), []byte(`{"build_id":"build-id"}`)); err == nil {
			t.Fatal("missing trusted invocation accepted")
		}
	}
}
