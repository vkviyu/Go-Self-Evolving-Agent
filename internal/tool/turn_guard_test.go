package tool

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/policy"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/memorystore"
)

type testTurnGuard struct {
	err   error
	calls int
}

func (guard *testTurnGuard) BeforeCall(context.Context, Call, Descriptor) error {
	guard.calls++
	return guard.err
}
func (guard *testTurnGuard) BeforeComplete(context.Context, session.ID, string) error {
	return guard.err
}

func TestPipelinePreconditionRunsBeforeApprovalAndExecutionButAfterReplay(t *testing.T) {
	registry := NewRegistry()
	executions := 0
	binding := testBinding("v1", "ok")
	binding.Executor = InvokeFunc(func(context.Context, json.RawMessage) (json.RawMessage, error) {
		executions++
		return json.RawMessage(`{"ok":true}`), nil
	})
	if _, err := registry.Activate(binding); err != nil {
		t.Fatal(err)
	}
	engine, err := policy.DefaultDeny(policy.Rule{ID: "approve", ToolPattern: "echo", CapabilityPattern: "tool.invoke", Effect: policy.EffectRequireApproval})
	if err != nil {
		t.Fatal(err)
	}
	events := memorystore.New()
	pipeline := mustPipeline(t, registry, engine, nil, events)
	precondition := &PreconditionError{Code: "test_precondition", Message: "register first"}
	guard := &testTurnGuard{err: precondition}
	ctx := WithTurnGuard(context.Background(), guard)
	call := Call{SessionID: "session", TurnID: "turn", CallID: "call", ToolName: "echo", Arguments: json.RawMessage(`{}`)}
	if _, err := pipeline.Execute(ctx, call); !errors.Is(err, precondition) {
		t.Fatalf("err=%v", err)
	}
	if executions != 0 {
		t.Fatal("executor ran before preconditions passed")
	}
	recorded, err := events.Load(ctx, "session", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(recorded) != 2 || recorded[0].Type != session.EventToolProposed || recorded[1].Type != session.EventToolFailed {
		t.Fatalf("precondition failure reached policy/approval/execution: %+v", recorded)
	}
	var failure struct {
		Code string `json:"error_code"`
	}
	if err := json.Unmarshal(recorded[1].Payload, &failure); err != nil || failure.Code != precondition.Code {
		t.Fatalf("failure=%+v err=%v", failure, err)
	}
	// Complete a different call normally, then replay it with a failing guard.
	engine, err = policy.DefaultDeny(policy.Rule{ID: "allow", ToolPattern: "echo", CapabilityPattern: "tool.invoke", Effect: policy.EffectAllow})
	if err != nil {
		t.Fatal(err)
	}
	pipeline = mustPipeline(t, registry, engine, nil, events)
	call.CallID = "successful"
	if _, err := pipeline.Execute(context.Background(), call); err != nil {
		t.Fatal(err)
	}
	if _, err := pipeline.Execute(ctx, call); err != nil {
		t.Fatal(err)
	}
	if executions != 1 || guard.calls != 1 {
		t.Fatalf("replayed execution=%d guard=%d", executions, guard.calls)
	}
}
