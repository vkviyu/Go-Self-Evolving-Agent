package app

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/runtimeowner"
)

type (
	scopeRecoveryRecorder struct {
		OrchestrationRepository
		reservations []orchestration.TeamID
		claims       []orchestration.TeamID
	}
)

func (s *scopeRecoveryRecorder) RecoverTeamReservations(_ context.Context, id orchestration.TeamID) (int, error) {
	s.reservations = append(s.reservations, id)
	return 1, nil
}

func (s *scopeRecoveryRecorder) ReapTeamTaskClaims(_ context.Context, id orchestration.TeamID, _ time.Time) (int, error) {
	s.claims = append(s.claims, id)
	return 1, nil
}

// TestSupervisorRecoveryScope protects other live Sessions sharing an ATE store
// from startup sweeps, including usage reservations and expired task claims.
func TestSupervisorRecoveryScope(t *testing.T) {
	fixture := &scopeRecoveryRecorder{}
	owned := []orchestration.TeamID{"team-a", "team-b"}
	scoped := scopedRepositories{OrchestrationRepository: fixture, teams: owned}
	teams, err := scoped.ListTeams(t.Context())
	if err != nil || !slices.Equal(teams, owned) {
		t.Fatalf("scope: %v, %v", teams, err)
	}
	teams[0] = "foreign-team"
	if n, err := scoped.RecoverReservations(t.Context()); err != nil || n != 2 {
		t.Fatalf("reservation recovery: %d, %v", n, err)
	}
	if n, err := scoped.ReapExpiredTaskClaims(t.Context(), time.Now()); err != nil || n != 2 {
		t.Fatalf("claim recovery: %d, %v", n, err)
	}
	if !slices.Equal(fixture.reservations, owned) || !slices.Equal(fixture.claims, owned) {
		t.Fatal("recovery escaped the runtime's owned Teams")
	}
}

// TestSessionExecutionOwnerDoesNotDependOnRuntimePath prevents two runtimes
// from executing one authoritative Session by selecting different data paths.
func TestSessionExecutionOwnerDoesNotDependOnRuntimePath(t *testing.T) {
	directory := t.TempDir()
	first := &App{
		config:    config.Config{DataDirectory: t.TempDir(), ATE: config.ATEConfig{Directory: directory}},
		authority: &authorityResources{storeID: "shared-store"},
	}
	second := &App{
		config:    config.Config{DataDirectory: t.TempDir(), ATE: config.ATEConfig{Directory: directory}},
		authority: &authorityResources{storeID: "shared-store"},
	}
	if err := first.ownSession("same-session"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, owner := range first.sessionOwners {
			_ = owner.Close()
		}
		for _, owner := range second.sessionOwners {
			_ = owner.Close()
		}
	})
	if err := second.ownSession("same-session"); !errors.Is(err, runtimeowner.ErrOwned) {
		t.Fatalf("second runtime acquired the same Session: %v", err)
	}
	if err := second.ownSession("different-session"); err != nil {
		t.Fatalf("independent Session was blocked: %v", err)
	}
}
