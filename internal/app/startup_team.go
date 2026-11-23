package app

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

// recoverRootSessionRequests runs with stateMu and root execution ownership.
// Native membership grants recovery scope for children; pending request text
// cannot grant ownership of arbitrary Sessions.
func (a *App) recoverRootSessionRequests(ctx context.Context, id session.ID) error {
	if a.sessionOwners[id] == nil {
		return errors.New("root request recovery requires execution ownership")
	}
	ids := []session.ID{id}
	root, err := a.repositories.RootThreadBySession(ctx, id)
	if err != nil && !errors.Is(err, orchestration.ErrNotFound) {
		return err
	}
	if err == nil {
		if root.SessionID != id {
			return errors.New("root request recovery Session mismatch")
		}
		agents, err := a.repositories.ListAgents(ctx, orchestration.AgentQuery{TeamID: root.TeamID})
		if err != nil {
			return err
		}
		for _, agent := range agents {
			if agent.TeamID != root.TeamID || agent.SessionID == "" {
				return errors.New("invalid Agent membership during request recovery")
			}
			ids = append(ids, agent.SessionID)
		}
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	// Acquire all target locks before advancing any saved plan.
	for _, target := range ids {
		if err := a.ownSession(target); err != nil {
			return fmt.Errorf("own pending request Session %s: %w", target, err)
		}
	}
	for _, target := range ids {
		if err := a.recoverSessionRequests(ctx, target); err != nil {
			return err
		}
	}
	return nil
}
