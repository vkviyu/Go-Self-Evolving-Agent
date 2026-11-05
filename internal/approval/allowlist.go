package approval

import (
	"context"
	"strings"

	"github.com/vkviyu/go-self-evolving-agent/internal/policy"
)

// ToolAllowlistApprover resolves only exact, host-selected tool names. It is
// intended for isolated automation where an interactive approval UI is not
// available; policy evaluation and approval audit events still occur in the
// Tool Pipeline.
type ToolAllowlistApprover struct {
	tools map[string]struct{}
}

// NewToolAllowlistApprover copies the supplied tool names into an immutable
// allowlist. Empty names are ignored and an empty allowlist denies everything.
func NewToolAllowlistApprover(toolNames ...string) *ToolAllowlistApprover {
	tools := make(map[string]struct{}, len(toolNames))
	for _, name := range toolNames {
		if name = strings.TrimSpace(name); name != "" {
			tools[name] = struct{}{}
		}
	}
	return &ToolAllowlistApprover{tools: tools}
}

func (approver *ToolAllowlistApprover) Approve(ctx context.Context, request policy.Request, _ policy.Decision) (bool, error) {
	if ctx != nil {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		default:
		}
	}
	if approver == nil {
		return false, nil
	}
	_, allowed := approver.tools[request.ToolName]
	return allowed, nil
}

var _ policy.Approver = (*ToolAllowlistApprover)(nil)
