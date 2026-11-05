package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/approval"
	"github.com/vkviyu/go-self-evolving-agent/internal/policy"
)

func TestSelectChatApproverKeepsHeadlessDefaultDeny(t *testing.T) {
	approver, err := selectChatApprover(false, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	approved, err := approver.Approve(context.Background(), policy.Request{ToolName: "workspace_write"}, policy.Decision{})
	if err != nil || approved {
		t.Fatalf("approved=%t err=%v", approved, err)
	}
}

func TestSelectChatApproverLimitsEvaluationTools(t *testing.T) {
	approver, err := selectChatApprover(false, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		tool string
		want bool
	}{
		{tool: "workspace_write", want: true},
		{tool: "workspace_edit", want: true},
		{tool: "shell_exec", want: true},
		{tool: "search_emails", want: false},
		{tool: "human_confirm", want: false},
		{tool: "merge_run", want: false},
		{tool: "merge_apply", want: true},
	} {
		approved, approveErr := approver.Approve(context.Background(), policy.Request{ToolName: test.tool}, policy.Decision{})
		if approveErr != nil {
			t.Fatalf("approve %q: %v", test.tool, approveErr)
		}
		if approved != test.want {
			t.Fatalf("approve %q = %t, want %t", test.tool, approved, test.want)
		}
	}
}

func TestSelectChatApproverUsesExclusiveInvocationToolAllowlist(t *testing.T) {
	approver, err := selectChatApprover(false, true, nil, "search_emails")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		tool string
		want bool
	}{
		{tool: "search_emails", want: true},
		{tool: "workspace_write", want: false},
		{tool: "workspace_edit", want: false},
		{tool: "shell_exec", want: false},
	} {
		approved, approveErr := approver.Approve(context.Background(), policy.Request{ToolName: test.tool}, policy.Decision{})
		if approveErr != nil {
			t.Fatalf("approve %q: %v", test.tool, approveErr)
		}
		if approved != test.want {
			t.Fatalf("approve %q = %t, want %t", test.tool, approved, test.want)
		}
	}
}

func TestSelectChatApproverRejectsEvaluationModeInTTY(t *testing.T) {
	if _, err := selectChatApprover(true, true, approval.NewBroker(1)); err == nil {
		t.Fatal("expected evaluation mode to be rejected for an interactive run")
	}
}

func TestSelectChatApproverUsesBrokerInTTY(t *testing.T) {
	broker := approval.NewBroker(1)
	approver, err := selectChatApprover(true, false, broker)
	if err != nil {
		t.Fatal(err)
	}
	if approver != broker {
		t.Fatalf("approver = %T, want the interactive broker", approver)
	}
}

func TestValidateEvaluationWorkspaceRejectsBroadRoots(t *testing.T) {
	if err := validateEvaluationWorkspace(t.TempDir()); err != nil {
		t.Fatalf("temporary evaluation workspace: %v", err)
	}
	root := string(filepath.Separator)
	if volume := filepath.VolumeName(t.TempDir()); volume != "" {
		root = volume + string(filepath.Separator)
	}
	if err := validateEvaluationWorkspace(root); err == nil {
		t.Fatal("expected filesystem root to be rejected")
	}
	home, err := os.UserHomeDir()
	if err == nil {
		if err := validateEvaluationWorkspace(home); err == nil {
			t.Fatal("expected user home to be rejected")
		}
	}
}
