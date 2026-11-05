package tool

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/policy"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/memorystore"
)

type receiptPolicy struct {
	effect policy.Effect
	calls  int
}

func TestTerminalReceiptDoesNotDowngradeJoinedInfrastructureFailure(t *testing.T) {
	for _, cause := range []error{
		errors.Join(ErrApprovalDenied, errors.New("audit write failed")),
		errors.Join(ErrPolicyDenied, errors.New("audit write failed")),
		&OutcomeUnknownError{CallID: "call", Tool: "echo", Cause: ErrPolicyDenied},
	} {
		if receipt := recoverableFailure(cause); receipt != nil {
			t.Fatalf("fatal failure gained a model-result receipt: cause=%v receipt=%+v", cause, receipt)
		}
	}
}

func (engine *receiptPolicy) Evaluate(context.Context, policy.Request) (policy.Decision, error) {
	engine.calls++
	return policy.Decision{Effect: engine.effect, Reason: "original decision"}, nil
}

type receiptApprover struct{ calls int }

func (approver *receiptApprover) Approve(context.Context, policy.Request, policy.Decision) (bool, error) {
	approver.calls++
	return false, nil
}

func TestPipelineReplaysTerminalModelToolErrorsWithoutNewExecution(t *testing.T) {
	for _, kind := range []string{"execution", "precondition", "policy_denied", "approval_denied"} {
		t.Run(kind, func(t *testing.T) {
			registry, store := NewRegistry(), memorystore.New()
			engine, approver := &receiptPolicy{effect: policy.EffectAllow}, &receiptApprover{}
			invocations := 0
			binding := testBinding("v1", "unused")
			binding.Descriptor.RiskClass = RiskWrite
			binding.Executor = InvokeFunc(func(context.Context, json.RawMessage) (json.RawMessage, error) {
				invocations++
				if kind == "precondition" {
					return nil, &PreconditionError{Code: "missing_fact", Message: "inspect the file first"}
				}
				return nil, errors.New("partial write failed")
			})
			if kind == "policy_denied" {
				engine.effect = policy.EffectDeny
			} else if kind == "approval_denied" {
				engine.effect = policy.EffectRequireApproval
			}
			if _, err := registry.Activate(binding); err != nil {
				t.Fatal(err)
			}
			call := Call{SessionID: "receipt", TurnID: "turn", CallID: "call", ToolName: "echo", Arguments: json.RawMessage(`{}`)}
			pipeline := mustPipeline(t, registry, engine, approver, store)
			_, first := pipeline.Execute(context.Background(), call)
			if first == nil {
				t.Fatal("expected a model-visible tool error")
			}
			sequence, _ := store.CurrentSequence(context.Background(), call.SessionID)
			// Rebuild the Pipeline, sharing only its persisted history.
			pipeline = mustPipeline(t, registry, engine, approver, store)
			_, replay := pipeline.Execute(context.Background(), call)
			if replay == nil || replay.Error() != first.Error() {
				t.Fatalf("replay changed the terminal outcome: first=%v replay=%v", first, replay)
			}
			if current, _ := store.CurrentSequence(context.Background(), call.SessionID); current != sequence || engine.calls != 1 || invocations > 1 || approver.calls > 1 {
				t.Fatalf("replay performed work: sequence=%d->%d policies=%d tools=%d approvals=%d", sequence, current, engine.calls, invocations, approver.calls)
			}
			call.Arguments = json.RawMessage(`{"changed":true}`)
			if _, err := pipeline.Execute(context.Background(), call); !errors.Is(err, ErrCallConflict) {
				t.Fatalf("failure receipt accepted changed arguments: %v", err)
			}
		})
	}
}
