package evolutiontools

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/approval"
	"github.com/vkviyu/go-self-evolving-agent/internal/policy"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/memorystore"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

func TestStrategyBindingsMatchProtocol(t *testing.T) {
	raw, err := os.ReadFile("testdata/bindings.json")
	if err != nil {
		t.Fatal(err)
	}
	var expected []tool.Descriptor
	if err := json.Unmarshal(raw, &expected); err != nil {
		t.Fatal(err)
	}
	bindings := StrategyBindings(nil, nil)
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

func TestStrictStrategyArguments(t *testing.T) {
	const valid = `{"candidate_id":"id"}`
	atLimit := valid + strings.Repeat(" ", maxStrategyArgumentBytes-len(valid))
	var input strategyInput
	if err := decodeEvolutionArgs([]byte(atLimit), &input); err != nil || input.CandidateID != "id" {
		t.Fatalf("valid input at limit rejected: %v", err)
	}
	for _, raw := range []string{
		`{"candidate_id":"id","unknown":true}`, valid + ` {}`, `{"candidate_id":`, `[]`, atLimit + " ",
	} {
		if err := decodeEvolutionArgs([]byte(raw), &input); err == nil {
			t.Fatalf("accepted invalid JSON: %.80s", raw)
		}
	}
}
