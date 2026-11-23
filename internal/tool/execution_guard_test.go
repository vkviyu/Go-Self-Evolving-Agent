package tool

import (
	"context"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

func TestExecutionBoundaryRequiresBindingForAssignedTaskSideEffects(t *testing.T) {
	err := (ExecutionBoundary{}).CheckCall(context.Background(), Call{
		Actor: orchestration.ActorScope{TaskID: "task-assigned"},
	}, Descriptor{Name: "write_file", RiskClass: RiskWrite})
	if err == nil {
		t.Fatal("assigned Task without execution binding was admitted")
	}
}

func TestExecutionBoundaryKeepsAdHocChildUnderPolicyBoundary(t *testing.T) {
	err := (ExecutionBoundary{}).CheckCall(context.Background(), Call{
		Actor: orchestration.ActorScope{RunID: "run-ad-hoc"},
	}, Descriptor{Name: "write_file", RiskClass: RiskWrite})
	if err != nil {
		t.Fatalf("ad-hoc child was rejected before policy/approval: %v", err)
	}
}
