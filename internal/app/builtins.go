package app

import (
	"github.com/vkviyu/go-self-evolving-agent/internal/artifact"
	"github.com/vkviyu/go-self-evolving-agent/internal/codingtools"
	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/policy"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

// buildPolicy creates the process policy once, at the composition boundary.
// Tool implementations do not carry authorization rules and cannot bypass
// this engine.
func buildPolicy(additionalTools []tool.Binding) (policy.Engine, error) {
	rules := []policy.Rule{
		{ID: "approve-strategy-propose", ToolPattern: "strategy_propose", CapabilityPattern: "tool.invoke", Effect: policy.EffectRequireApproval, Reason: "retain a bounded strategy candidate; no automatic activation"},
		{ID: "allow-strategy-inspect", ToolPattern: "strategy_inspect", CapabilityPattern: "tool.invoke", Effect: policy.EffectAllow, Reason: "inspect this root session's candidate and test evidence"},
		{ID: "approve-strategy-shadow", ToolPattern: "strategy_shadow", CapabilityPattern: "tool.invoke", Effect: policy.EffectRequireApproval, Reason: "run bounded model comparisons against user-registered fixtures; requests consume the current Team budget"},
		{ID: "approve-strategy-publish", ToolPattern: "strategy_publish", CapabilityPattern: "tool.invoke", Effect: policy.EffectRequireApproval, Reason: "publish the exact tested strategy for future Turns with this baseline config; never relax permissions"},
		{ID: "approve-plugin-propose", ToolPattern: "plugin_propose", CapabilityPattern: "tool.invoke", Effect: policy.EffectRequireApproval, Reason: "compile untrusted source in an isolated worker and retain a candidate; no automatic publication"},
		{ID: "allow-plugin-inspect", ToolPattern: "plugin_inspect", CapabilityPattern: "tool.invoke", Effect: policy.EffectAllow, Reason: "inspect this session's own immutable build evidence"},
		{ID: "approve-plugin-publish", ToolPattern: "plugin_publish", CapabilityPattern: "tool.invoke", Effect: policy.EffectRequireApproval, Reason: "review generated source/tests before promoting the exact candidate; activation changes future toolsets"},
		{ID: "approve-extension-invoke", ToolPattern: "extension_*", CapabilityPattern: "tool.invoke", Effect: policy.EffectRequireApproval, Reason: "explicit invocation of a host-published generated WASM extension"},
		{ID: "allow-runtime-diagnostics", ToolPattern: diagnosticsToolName, CapabilityPattern: "tool.invoke", Effect: policy.EffectAllow, Reason: "built-in read-only health check"},
		{ID: "allow-goal-report", ToolPattern: "goal_report", CapabilityPattern: "tool.invoke", Effect: policy.EffectAllow, Reason: "durable Goal Mode self-assessment"},
		{ID: "allow-goal-start", ToolPattern: "goal_start", CapabilityPattern: "tool.invoke", Effect: policy.EffectAllow, Reason: "task contract registration; grants no execution authority"},
		{ID: "allow-runtime-budget", ToolPattern: "runtime_budget", CapabilityPattern: "tool.invoke", Effect: policy.EffectAllow, Reason: "read-only durable Team budget"},
		{ID: "approve-human-confirm", ToolPattern: "human_confirm", CapabilityPattern: "tool.invoke", Effect: policy.EffectRequireApproval, Reason: "Flow requested explicit human intervention"},
		{ID: "allow-flow-list", ToolPattern: "flow_list", CapabilityPattern: "tool.invoke", Effect: policy.EffectAllow, Reason: "read-only host Flow catalog"},
		{ID: "allow-flow-run", ToolPattern: "flow_run", CapabilityPattern: "tool.invoke", Effect: policy.EffectAllow, Reason: "host-registered Flow whose actions remain policy-controlled"},
		{ID: "allow-agent-spawn", ToolPattern: "agent_spawn", CapabilityPattern: "tool.invoke", Effect: policy.EffectAllow, Reason: "Supervisor enforces Agent spawn limits"},
		{ID: "allow-agent-continue", ToolPattern: "agent_continue", CapabilityPattern: "tool.invoke", Effect: policy.EffectAllow, Reason: "Supervisor preserves the durable child session boundary"},
		{ID: "allow-agent-resume", ToolPattern: "agent_resume", CapabilityPattern: "tool.invoke", Effect: policy.EffectAllow, Reason: "resume a child paused by an external recovery condition"},
		{ID: "allow-agent-list", ToolPattern: "agent_list", CapabilityPattern: "tool.invoke", Effect: policy.EffectAllow, Reason: "read-only Agent inspection"},
		{ID: "allow-agent-wait", ToolPattern: "agent_wait", CapabilityPattern: "tool.invoke", Effect: policy.EffectAllow, Reason: "wait for durable Agent state"},
		{ID: "allow-agent-interrupt", ToolPattern: "agent_interrupt", CapabilityPattern: "tool.invoke", Effect: policy.EffectAllow, Reason: "Supervisor handles Agent cancellation"},
		{ID: "allow-agent-message", ToolPattern: "agent_message", CapabilityPattern: "tool.invoke", Effect: policy.EffectAllow, Reason: "durable parent-child mailbox message"},
		{ID: "allow-task-create", ToolPattern: "task_create", CapabilityPattern: "tool.invoke", Effect: policy.EffectAllow, Reason: "durable Team task creation"},
		{ID: "allow-task-update", ToolPattern: "task_update", CapabilityPattern: "tool.invoke", Effect: policy.EffectAllow, Reason: "edit draft and ready task plans"},
		{ID: "allow-task-refine", ToolPattern: "task_refine", CapabilityPattern: "tool.invoke", Effect: policy.EffectAllow, Reason: "bounded additive task refinement"},
		{ID: "allow-task-list", ToolPattern: "task_list", CapabilityPattern: "tool.invoke", Effect: policy.EffectAllow, Reason: "read-only Team task inspection"},
		{ID: "allow-task-transition", ToolPattern: "task_transition", CapabilityPattern: "tool.invoke", Effect: policy.EffectAllow, Reason: "validated durable task transition"},
		{ID: "allow-task-verify", ToolPattern: "task_verify", CapabilityPattern: "tool.invoke", Effect: policy.EffectAllow, Reason: "Supervisor only accepts verdicts from the separate reviewer of a task in verification"},
		{ID: "allow-task-claim", ToolPattern: "task_claim", CapabilityPattern: "tool.invoke", Effect: policy.EffectAllow, Reason: "durable task claim"},
		{ID: "allow-task-release", ToolPattern: "task_release", CapabilityPattern: "tool.invoke", Effect: policy.EffectAllow, Reason: "durable task release"},
		{ID: "allow-task-reassign", ToolPattern: "task_reassign", CapabilityPattern: "tool.invoke", Effect: policy.EffectAllow, Reason: "retry only explicitly retryable task failures"},
		{ID: "allow-task-add-dependency", ToolPattern: "task_add_dependency", CapabilityPattern: "tool.invoke", Effect: policy.EffectAllow, Reason: "durable task dependency update"},
		{ID: "allow-task-remove-dependency", ToolPattern: "task_remove_dependency", CapabilityPattern: "tool.invoke", Effect: policy.EffectAllow, Reason: "durable task dependency update"},
		{ID: "allow-merge-list", ToolPattern: "merge_list", CapabilityPattern: "tool.invoke", Effect: policy.EffectAllow, Reason: "read-only merge queue inspection"},
		{ID: "approve-merge-requeue", ToolPattern: "merge_requeue", CapabilityPattern: "tool.invoke", Effect: policy.EffectRequireApproval, Reason: "requeueing a merge changes workspace coordination state"},
		{ID: "approve-merge-run", ToolPattern: "merge_run", CapabilityPattern: "tool.invoke", Effect: policy.EffectRequireApproval, Reason: "applying a merge changes the workspace"},
		{ID: "allow-workspace-read", ToolPattern: "workspace_read", CapabilityPattern: "tool.invoke", Effect: policy.EffectAllow, Reason: "read-only workspace inspection"},
		{ID: "allow-workspace-list", ToolPattern: "workspace_list", CapabilityPattern: "tool.invoke", Effect: policy.EffectAllow, Reason: "read-only workspace inspection"},
		{ID: "allow-workspace-glob", ToolPattern: "workspace_glob", CapabilityPattern: "tool.invoke", Effect: policy.EffectAllow, Reason: "read-only workspace inspection"},
		{ID: "allow-workspace-grep", ToolPattern: "workspace_grep", CapabilityPattern: "tool.invoke", Effect: policy.EffectAllow, Reason: "read-only workspace inspection"},
		{ID: "allow-workspace-diagnostics", ToolPattern: "workspace_diagnostics", CapabilityPattern: "tool.invoke", Effect: policy.EffectAllow, Reason: "read-only language-server diagnostics"},
		{ID: "allow-artifact-read", ToolPattern: "artifact_read", CapabilityPattern: "tool.invoke", Effect: policy.EffectAllow, Reason: "read-only bounded artifact retrieval"},
		{ID: "approve-workspace-write", ToolPattern: codingtools.WriteToolName, CapabilityPattern: "tool.invoke", Effect: policy.EffectRequireApproval, Reason: "writing files changes the workspace"},
		{ID: "approve-workspace-edit", ToolPattern: codingtools.EditToolName, CapabilityPattern: "tool.invoke", Effect: policy.EffectRequireApproval, Reason: "editing files changes the workspace"},
		{ID: "approve-shell-exec", ToolPattern: codingtools.ShellToolName, CapabilityPattern: "tool.invoke", Effect: policy.EffectRequireApproval, Reason: "shell commands can modify the system"},
	}
	for _, binding := range additionalTools {
		rules = append(rules, policy.Rule{
			ID:                "approve-invocation-tool-" + binding.Descriptor.Name,
			ToolPattern:       binding.Descriptor.Name,
			CapabilityPattern: "tool.invoke",
			Effect:            policy.EffectRequireApproval,
			Reason:            "invocation-scoped external tool",
		})
	}
	return policy.DefaultDeny(rules...)
}

// buildBuiltinBindings returns the complete first-party tool catalog. The
// registry publishes this slice atomically so no turn can observe a partial
// startup catalog.
func buildBuiltinBindings(resolved config.Config, workspace *codingtools.Workspace, artifacts *artifact.Store) []tool.Binding {
	if workspace == nil {
		return nil
	}
	return append([]tool.Binding{diagnosticsBinding(resolved), artifactReadBinding(artifacts)}, workspace.Bindings()...)
}
