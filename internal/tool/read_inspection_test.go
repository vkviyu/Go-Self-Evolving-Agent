package tool

import (
	"encoding/json"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/policy"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/memorystore"
)

func TestReadInspectionRequiresNativeBindingAndAllowWithoutSideEffects(t *testing.T) {
	binding := testBinding("builtin-test", "ignored")
	binding.Descriptor.RiskClass = RiskRead
	binding.ImplementationID = "native-test"
	registry := identityRegistry(t, binding)
	call := Call{
		SessionID: "session", TurnID: "turn", CallID: "metadata", ToolName: binding.Descriptor.Name,
		Arguments: json.RawMessage(`{"path":"docs/design.md"}`), Registry: registry.Snapshot(),
		RuntimeSnapshotID: requireSnapshotID(t, registry),
	}
	expected := SnapshotEntry{Descriptor: binding.Descriptor, Implementation: binding.ImplementationID}
	for _, effect := range []policy.Effect{policy.EffectAllow, policy.EffectDeny, policy.EffectRequireApproval} {
		t.Run(string(effect), func(t *testing.T) {
			engine, err := policy.NewStaticEngine(effect)
			if err != nil {
				t.Fatal(err)
			}
			approver := &testApprover{approved: true}
			events := memorystore.New()
			pipeline := mustPipeline(t, registry, engine, approver, events)
			if got := pipeline.AllowsReadInspection(t.Context(), call, expected); got != (effect == policy.EffectAllow) {
				t.Fatalf("read inspection allowed = %v, policy effect = %s", got, effect)
			}
			mismatch := expected
			mismatch.Implementation = "other-executor"
			if pipeline.AllowsReadInspection(t.Context(), call, mismatch) {
				t.Fatal("authorized metadata with a different executable binding")
			}
			recorded, err := events.Load(t.Context(), call.SessionID, 0, 10)
			if err != nil || len(recorded) != 0 || approver.called {
				t.Fatalf("metadata check recorded events or requested approval: %v, %v, %v", recorded, approver.called, err)
			}
		})
	}
}
