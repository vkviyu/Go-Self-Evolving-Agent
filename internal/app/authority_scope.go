package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"slices"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/runtimeowner"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

type (
	// scopedRepositories carries execution ownership references, not business
	// projections. Shared ATE history does not grant a runtime every Team.
	scopedRepositories struct {
		OrchestrationRepository
		teams []orchestration.TeamID
	}
	teamRecoveryRepository interface {
		RecoverTeamReservations(context.Context, orchestration.TeamID) (int, error)
		ReapTeamTaskClaims(context.Context, orchestration.TeamID, time.Time) (int, error)
	}
)

// ownSession is called while stateMu is held. The lock is keyed by the stable
// Session identity at the shared store, independently of the local data path.
func (a *App) ownSession(id session.ID) error {
	if id == "" || a.authority == nil {
		return errors.New("session identity and ATE authority are required for execution ownership")
	}
	if a.sessionOwners == nil {
		a.sessionOwners = make(map[session.ID]*runtimeowner.Lock)
	}
	if a.sessionOwners[id] != nil {
		return nil
	}
	if a.supervisor != nil {
		return errors.New("cannot attach another session after the Supervisor starts")
	}
	directory, err := filepath.EvalSymlinks(a.config.ATE.Directory)
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(string(a.authority.storeID) + "\x00" + string(id)))
	lock, err := runtimeowner.Acquire(filepath.Join(filepath.Dir(directory), ".gsea-runtime-owners", hex.EncodeToString(sum[:])))
	if err != nil {
		return err
	}
	a.sessionOwners[id] = lock
	return nil
}

// supervisorRepositories is called while stateMu is held, after a root Session
// has acquired its execution lease and resolved its authoritative Team ID.
func (a *App) supervisorRepositories() (OrchestrationRepository, error) {
	if len(a.ownedTeams) == 0 {
		return nil, errors.New("ensure a root session before starting its Supervisor")
	}
	teams := make([]orchestration.TeamID, 0, len(a.ownedTeams))
	for id := range a.ownedTeams {
		teams = append(teams, id)
	}
	slices.Sort(teams)
	return scopedRepositories{OrchestrationRepository: a.repositories, teams: teams}, nil
}

// ListTeams exposes only the Teams whose root Sessions this runtime owns.
func (r scopedRepositories) ListTeams(ctx context.Context) ([]orchestration.TeamID, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return slices.Clone(r.teams), nil
}

// RecoverReservations cannot sweep another runtime's in-flight model requests.
func (r scopedRepositories) RecoverReservations(ctx context.Context) (int, error) {
	recovery, ok := r.OrchestrationRepository.(teamRecoveryRepository)
	if !ok {
		return 0, errors.New("authority repository lacks scoped recovery")
	}
	count := 0
	for _, team := range r.teams {
		n, err := recovery.RecoverTeamReservations(ctx, team)
		if err != nil {
			return count, err
		}
		count += n
	}
	return count, nil
}

// ReapExpiredTaskClaims limits automatic claim repair to the owned Teams.
func (r scopedRepositories) ReapExpiredTaskClaims(ctx context.Context, now time.Time) (int, error) {
	recovery, ok := r.OrchestrationRepository.(teamRecoveryRepository)
	if !ok {
		return 0, errors.New("authority repository lacks scoped claim recovery")
	}
	count := 0
	for _, team := range r.teams {
		n, err := recovery.ReapTeamTaskClaims(ctx, team, now)
		if err != nil {
			return count, err
		}
		count += n
	}
	return count, nil
}
