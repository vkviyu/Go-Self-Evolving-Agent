package approval

import (
	"context"
	"errors"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/policy"
)

func TestToolAllowlistApproverUsesExactToolNames(t *testing.T) {
	approver := NewToolAllowlistApprover("workspace_write", " shell_exec ", "")
	for _, test := range []struct {
		name string
		want bool
	}{
		{name: "workspace_write", want: true},
		{name: "shell_exec", want: true},
		{name: "workspace_edit", want: false},
		{name: "workspace_write_extra", want: false},
		{name: "human_confirm", want: false},
	} {
		approved, err := approver.Approve(context.Background(), policy.Request{ToolName: test.name}, policy.Decision{Effect: policy.EffectRequireApproval})
		if err != nil {
			t.Fatalf("approve %q: %v", test.name, err)
		}
		if approved != test.want {
			t.Fatalf("approve %q = %t, want %t", test.name, approved, test.want)
		}
	}
}

func TestToolAllowlistApproverHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	approved, err := NewToolAllowlistApprover("workspace_write").Approve(ctx, policy.Request{ToolName: "workspace_write"}, policy.Decision{})
	if approved || !errors.Is(err, context.Canceled) {
		t.Fatalf("approved=%t err=%v", approved, err)
	}
}

func TestNilToolAllowlistApproverDenies(t *testing.T) {
	var approver *ToolAllowlistApprover
	approved, err := approver.Approve(context.Background(), policy.Request{ToolName: "workspace_write"}, policy.Decision{})
	if err != nil || approved {
		t.Fatalf("approved=%t err=%v", approved, err)
	}
}
