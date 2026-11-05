package app

import (
	"context"
	"errors"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentmanager"
	"github.com/vkviyu/go-self-evolving-agent/internal/approval"
	"github.com/vkviyu/go-self-evolving-agent/internal/lsp"
	"github.com/vkviyu/go-self-evolving-agent/internal/plugin/wasm"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/sqlitestore"
)

func closeOpenResources(database *sqlitestore.Database, wasmRuntime *wasm.Runtime, approvalBroker *approval.Broker, diagnostics *lsp.Manager) {
	if diagnostics != nil {
		_ = diagnostics.Close()
	}
	if approvalBroker != nil {
		_ = approvalBroker.Close()
	}
	if wasmRuntime != nil {
		_ = wasmRuntime.Close(context.Background())
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
		plugins := a.plugins
		diagnostics := a.diagnostics
		approvalBroker := a.approval
		wasmRuntime := a.wasm
		database := a.database
		a.stateMu.Unlock()
		if !a.joinEvolution() {
			// Do not release the database, modules or owner while a worker may
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
			closeErrs = append(closeErrs, err)
		}
		if plugins != nil {
			closeErrs = append(closeErrs, plugins.Close(context.Background()))
		}
		if diagnostics != nil {
			closeErrs = append(closeErrs, diagnostics.Close())
		}
		if approvalBroker != nil {
			closeErrs = append(closeErrs, approvalBroker.Close())
		}
		if wasmRuntime != nil {
			closeErrs = append(closeErrs, wasmRuntime.Close(context.Background()))
		}
		if database != nil {
			closeErrs = append(closeErrs, database.Close())
		}
		closeErrs = append(closeErrs, a.domain.Close(), a.owner.Close())
		a.closeErr = errors.Join(closeErrs...)
	})
	return a.closeErr
}

// retainUntilExit keeps runtime ownership and workspace leases until the
// process exits, because a worker that ignored cancellation may still write.
func (a *App) retainUntilExit() {
	a.owner.RetainUntilExit()
	a.domain.RetainUntilExit()
}
