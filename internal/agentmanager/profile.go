package agentmanager

import (
	"context"
	"fmt"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentprofile"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

func (supervisor *Supervisor) agentProfile(agent orchestration.AgentSnapshot) (agentprofile.Profile, bool) {
	if len(agent.ProfileSnapshot) > 0 {
		profile, err := agentprofile.Decode(agent.ProfileSnapshot, agent.ProfileRevision)
		return profile, err == nil
	}
	// Legacy metadata is available to control-plane queries. The production
	// child Runner separately refuses execution without a durable snapshot.
	return supervisor.profileRevision(agent.ProfileRevision)
}

func (supervisor *Supervisor) actorProfile(ctx context.Context, actor orchestration.ActorScope) (agentprofile.Profile, bool, error) {
	agent, err := supervisor.agents.GetAgent(ctx, actor.AgentID)
	if err != nil {
		return agentprofile.Profile{}, false, err
	}
	if agent.TeamID != actor.TeamID || agent.ProfileRevision != actor.ProfileRevision {
		return agentprofile.Profile{}, false, fmt.Errorf("Agent profile scope mismatch")
	}
	profile, ok := supervisor.agentProfile(agent)
	if !ok && (supervisor.profiles != nil || len(agent.ProfileSnapshot) > 0) {
		return profile, false, agentprofile.ErrSnapshot
	}
	return profile, ok, nil
}

func inheritanceRank(policy string) int {
	switch agentprofile.ContextInheritance(policy) {
	case agentprofile.InheritNone:
		return 0
	case agentprofile.InheritSummary:
		return 1
	case agentprofile.InheritRecent:
		return 2
	case agentprofile.InheritFull:
		return 3
	default:
		return -1
	}
}
