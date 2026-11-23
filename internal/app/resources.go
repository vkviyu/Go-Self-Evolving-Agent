package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentflow"
	"github.com/vkviyu/go-self-evolving-agent/internal/approval"
	"github.com/vkviyu/go-self-evolving-agent/internal/artifact"
	"github.com/vkviyu/go-self-evolving-agent/internal/codingtools"
	"github.com/vkviyu/go-self-evolving-agent/internal/plugin"
	"github.com/vkviyu/go-self-evolving-agent/internal/plugin/wasm"
	"github.com/vkviyu/go-self-evolving-agent/internal/policy"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacecoord"
)

// FlowCatalog returns the host-owned registry of safe, named Flows exposed to
// Agents through flow_list and flow_run.
func (a *App) FlowCatalog() *agentflow.Catalog {
	if a == nil {
		return nil
	}
	return a.flowCatalog
}

// Registry returns the process registry for activation services.
func (a *App) Registry() *tool.Registry {
	if a == nil {
		return nil
	}
	return a.registry
}

// Pipeline returns the only supported tool execution boundary.
func (a *App) Pipeline() *tool.Pipeline {
	if a == nil {
		return nil
	}
	return a.pipeline
}

// FlowRuntime returns the host-owned composable Flow executor. Flow side
// effects are routed back through Pipeline and never invoke Supervisor or
// workspace services directly.
func (a *App) FlowRuntime() *agentflow.Runtime {
	if a == nil {
		return nil
	}
	return a.flows
}

// Approval returns the interactive approval broker used by write and execute tools.
func (a *App) Approval() *approval.Broker {
	if a == nil {
		return nil
	}
	return a.approval
}

// WorkspaceDirectory returns the canonical directory exposed to coding tools.
func (a *App) WorkspaceDirectory() string {
	if a == nil {
		return ""
	}
	return a.config.WorkspaceDirectory
}

// WorkspaceCoordinator exposes durable Agent workspace views and merge queue
// operations to composition-root callers and headless control planes.
func (a *App) WorkspaceCoordinator() *workspacecoord.Coordinator {
	if a == nil {
		return nil
	}
	return a.coordinator
}

// Artifacts returns the default content-addressed store used by workspace
// merges and other durable large-object references.
func (a *App) Artifacts() *artifact.Store {
	if a == nil {
		return nil
	}
	return a.artifacts
}

// NewMergeExecutor creates the serialized workspace merge worker. A nil
// artifact reader uses the private, durable workspace artifact directory;
// callers can provide a content-addressed or remote reader when needed.
// Approver decides snapshot apply plans unless auto merge is configured;
// without one, snapshot merges stay queued for a session that can ask.
func (a *App) NewMergeExecutor(artifacts workspacecoord.ArtifactReader, approver policy.Approver) (*workspacecoord.MergeExecutor, error) {
	if a == nil || a.repositories == nil || a.coordinator == nil {
		return nil, errors.New("application workspace coordination is unavailable")
	}
	if artifacts == nil {
		if a.artifacts == nil {
			return nil, errors.New("application artifact store is unavailable")
		}
		artifacts = a.artifacts
	}
	return a.newMergeExecutor(artifacts, approver)
}

// newMergeExecutor creates a merge worker whose snapshot merges follow the
// configured auto-merge setting and otherwise ask approver. A configured
// post-merge check gates the tasks of every applied merge.
func (a *App) newMergeExecutor(artifacts workspacecoord.ArtifactReader, approver policy.Approver) (*workspacecoord.MergeExecutor, error) {
	executor, err := workspacecoord.NewMergeExecutor(a.repositories, a.coordinator, a.coordinator.Worktrees, artifacts, a.repositories)
	if err != nil {
		return nil, err
	}
	executor.AutoMerge = a.config.Agents.AutoMerge
	executor.Approver = mergeApprover(approver, a.repositories)
	if command := a.config.Agents.PostMergeCheck; command != "" && a.workspace != nil {
		executor.Verifier = postMergeCheck{workspace: a.workspace, command: command, timeout: a.config.Agents.PostMergeCheckTimeout}
	}
	return executor, nil
}

// postMergeCheck runs [agents].post_merge_check on the main workspace.
type postMergeCheck struct {
	workspace *codingtools.Workspace
	command   string
	timeout   time.Duration
}

// VerifyMerge runs the configured check once per applied merge.
func (check postMergeCheck) VerifyMerge(ctx context.Context, _ workspacecoord.MergeRequest) error {
	return check.workspace.RunCheck(ctx, check.command, check.timeout)
}

func (check postMergeCheck) VerificationIdentity() string {
	data, _ := json.Marshal(struct {
		Protocol, Command string
		Timeout           time.Duration
	}{"post-merge-readonly-v1", check.command, check.timeout})
	digest := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(digest[:])
}

// WASM returns the sandbox runtime used to compile immutable revisions.
func (a *App) WASM() *wasm.Runtime {
	if a == nil {
		return nil
	}
	return a.wasm
}

// Plugins returns the staged immutable revision lifecycle manager.
func (a *App) Plugins() *plugin.Manager {
	if a == nil {
		return nil
	}
	return a.plugins
}
