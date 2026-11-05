package tool

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/policy"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/memorystore"
)

func TestPipelineEnforcesSchemaBeforeApprovalAndExecution(t *testing.T) {
	registry := NewRegistry()
	calls := 0
	binding := Binding{Descriptor: Descriptor{Name: "bounded", Description: "bounded", Revision: "v1", RiskClass: RiskWrite, InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["n"],"properties":{"n":{"type":"integer","minimum":0,"maximum":511}}}`)}, Executor: InvokeFunc(func(context.Context, json.RawMessage) (json.RawMessage, error) {
		calls++
		return json.RawMessage(`{}`), nil
	})}
	if _, err := registry.ActivateMany(binding); err != nil {
		t.Fatal(err)
	}
	engine, err := policy.NewStaticEngine(policy.EffectRequireApproval)
	if err != nil {
		t.Fatal(err)
	}
	pipeline, err := NewPipeline(PipelineConfig{Registry: registry, Policy: engine, Events: memorystore.New()})
	if err != nil {
		t.Fatal(err)
	}
	for index, raw := range []string{`{}`, `{"n":512}`, `{"n":0.5}`, `{"n":1,"extra":true}`, `[]`} {
		_, err := pipeline.Execute(context.Background(), Call{SessionID: "s", TurnID: "t", ToolName: "bounded", Arguments: json.RawMessage(raw)})
		var precondition *PreconditionError
		if !errors.As(err, &precondition) || precondition.Code != "invalid_arguments" {
			t.Fatalf("case %d error=%v", index, err)
		}
	}
	if calls != 0 {
		t.Fatal("invalid arguments reached executor")
	}
}

func TestToolSchemaNeverFetchesExternalReferences(t *testing.T) {
	if _, err := compileInputSchema(json.RawMessage(`{"type":"object","properties":{"secret":{"$ref":"file:///etc/passwd"}}}`)); err == nil {
		t.Fatal("external schema reference accepted")
	}
	if _, err := compileInputSchema(json.RawMessage(`{"type":"object","$defs":{"n":{"type":"integer"}},"properties":{"n":{"$ref":"#/$defs/n"}}}`)); err != nil {
		t.Fatal(err)
	}
}
