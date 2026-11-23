package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

// resumeOwnedSessionMaintenance requires stateMu and follows request recovery.
// Holding the lock retains the execution lease until both operations finish.
func (a *App) resumeOwnedSessionMaintenance(ctx context.Context, id session.ID) error {
	if a.closed || a.repositories == nil || a.sessionOwners[id] == nil {
		return errors.New("session maintenance requires its execution-owner lease")
	}
	if err := a.repositories.ResumeAbandonments(ctx, id); err != nil {
		return fmt.Errorf("recover session abandonment: %w", err)
	}
	if err := a.repositories.ResumeRetentions(ctx, id); err != nil {
		return fmt.Errorf("recover session archival: %w", err)
	}
	return nil
}
