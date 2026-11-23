package tool

import (
	"context"

	"github.com/vkviyu/go-self-evolving-agent/internal/policy"
)

// AllowsReadInspection checks whether a host metadata inspection has the same
// permission as an ordinary read of this resource. It never starts a tool,
// consumes its quota, writes an invocation event, or prompts for approval.
func (p *Pipeline) AllowsReadInspection(ctx context.Context, call Call, expected SnapshotEntry) bool {
	if p == nil || ctx == nil || ctx.Err() != nil || call.Registry == nil || call.CallID == "" || p.validate() != nil {
		return false
	}
	if err := validateCall(&call); err != nil {
		return false
	}
	snapshot, binding, err := p.resolve(call)
	if err != nil || binding.Descriptor.RiskClass != RiskRead || !expected.Matches(binding) {
		return false
	}
	if id, err := snapshot.ID(); err != nil || id != call.RuntimeSnapshotID {
		return false
	}
	if err := snapshot.validateArguments(binding, call.Arguments); err != nil {
		return false
	}
	for _, guard := range p.guards {
		if err := guard.CheckCall(ctx, call, binding.Descriptor); err != nil {
			return false
		}
	}
	if err := checkCallPreconditions(ctx, call, binding.Descriptor); err != nil {
		return false
	}
	decision, err := p.policy.Evaluate(ctx, makePolicyRequest(call, binding))
	return err == nil && decision.Effect == policy.EffectAllow
}
