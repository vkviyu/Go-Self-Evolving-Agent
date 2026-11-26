package app

import (
	"errors"
	"fmt"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentmanager"
	"github.com/vkviyu/go-self-evolving-agent/internal/approval"
	"github.com/vkviyu/go-self-evolving-agent/internal/runtimestore"
)

// closeOpenResources releases the broker and database after a failed open.
func closeOpenResources(database *runtimestore.Database, approvalBroker *approval.Broker) {
	if approvalBroker != nil {
		_ = approvalBroker.Close()
	}
	if database != nil {
		_ = database.Close()
	}
}

// Close stops owned work before releasing resources, exactly once. A worker
// timeout retains resources and ownership until process exit; retries return
// the same error rather than assuming the worker has since stopped.
func (a *App) Close() error {
	if a == nil {
		return nil
	}
	a.closeOnce.Do(func() {
		a.stateMu.Lock()
		a.closed = true
		if a.evolutionCancel != nil {
			a.evolutionCancel()
		}
		supervisor := a.supervisor
		approvalBroker := a.approval
		database := a.database
		a.stateMu.Unlock()
		if !a.joinEvolution() {
			// Do not release the database or owner while a worker may
			// still commit evidence. A wedged provider requires process exit.
			a.retainUntilExit()
			a.closeErr = errors.New("evolution worker did not stop; runtime ownership retained until process exit")
			return
		}

		var closeErrs []error
		if supervisor != nil {
			err := supervisor.Close()
			if errors.Is(err, agentmanager.ErrSupervisorCloseTimeout) {
				// Child runners and monitor workers may still access every
				// resource below, including the durable evidence store and
				// the workspace leases that keep other instances out.
				a.retainUntilExit()
				a.closeErr = err
				return
			}
			if err != nil {
				closeErrs = append(closeErrs, fmt.Errorf("close Agent supervisor: %w", err))
			}
		}
		if approvalBroker != nil {
			if err := approvalBroker.Close(); err != nil {
				closeErrs = append(closeErrs, fmt.Errorf("close approval broker: %w", err))
			}
		}
		if a.temporalProcess != nil {
			if err := a.temporalProcess.Close(); err != nil {
				closeErrs = append(closeErrs, fmt.Errorf("close ATE process: %w", err))
			}
		}
		if database != nil {
			if err := database.Close(); err != nil {
				closeErrs = append(closeErrs, fmt.Errorf("close runtime database: %w", err))
			}
		}
		for _, owner := range a.sessionOwners {
			if owner != nil {
				if err := owner.Close(); err != nil {
					closeErrs = append(closeErrs, fmt.Errorf("release session owner: %w", err))
				}
			}
		}
		if a.domain != nil {
			if err := a.domain.Close(); err != nil {
				closeErrs = append(closeErrs, fmt.Errorf("close workspace domain: %w", err))
			}
		}
		if a.owner != nil {
			if err := a.owner.Close(); err != nil {
				closeErrs = append(closeErrs, fmt.Errorf("release runtime owner: %w", err))
			}
		}
		a.closeErr = errors.Join(closeErrs...)
	})
	return a.closeErr
}

// retainUntilExit keeps runtime ownership and workspace leases until the
// process exits, because a worker that ignored cancellation may still write.
func (a *App) retainUntilExit() {
	if a.owner != nil {
		a.owner.RetainUntilExit()
	}
	if a.domain != nil {
		a.domain.RetainUntilExit()
	}
	for _, owner := range a.sessionOwners {
		if owner != nil {
			owner.RetainUntilExit()
		}
	}
}
