package atestore

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/ateauthority"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

// GetTeam reads a team snapshot, including its current version for subsequent transitions.
func (store *Store) GetTeam(ctx context.Context, id orchestration.TeamID) (result orchestration.TeamSnapshot, err error) {
	if err = id.Validate(); err != nil {
		return
	}
	err = store.authority.View(ctx, func(tx *ateauthority.Tx) error {
		var e error
		result, e = getRecord[orchestration.TeamSnapshot](tx, "team", string(id))
		return e
	})
	return
}

// ListTeams returns team IDs recorded by the orchestration authority.
func (store *Store) ListTeams(ctx context.Context) (result []orchestration.TeamID, err error) {
	err = store.authority.View(ctx, func(tx *ateauthority.Tx) error {
		teams, e := listRecords[orchestration.TeamSnapshot](tx, "team")
		if e != nil {
			return e
		}
		times := make(map[orchestration.TeamID]time.Time, len(teams))
		result = make([]orchestration.TeamID, 0, len(teams))
		for _, team := range teams {
			created, e := getRecord[time.Time](tx, "team-created", string(team.ID))
			if e != nil {
				return e
			}
			times[team.ID] = created
			result = append(result, team.ID)
		}
		sort.Slice(result, func(i, j int) bool {
			if times[result[i]].Equal(times[result[j]]) {
				return result[i] < result[j]
			}
			return times[result[i]].Before(times[result[j]])
		})
		return nil
	})
	return
}
func requireActiveTeam(tx *ateauthority.Tx, id orchestration.TeamID) error {
	team, err := getRecord[orchestration.TeamSnapshot](tx, "team", string(id))
	if err != nil {
		return err
	}
	if team.State != orchestration.TeamActive {
		return fmt.Errorf("%w: Team %s is %s", orchestration.ErrTeamNotActive, id, team.State)
	}
	return nil
}

// TransitionTeam checks the team's version and records an allowed lifecycle transition.
func (store *Store) TransitionTeam(ctx context.Context, transition orchestration.TeamTransition) (result orchestration.TeamSnapshot, err error) {
	if err = transition.TeamID.Validate(); err != nil {
		return
	}
	now := time.Now().UTC()
	err = store.authority.Update(ctx, func(tx *ateauthority.Tx) error {
		var e error
		result, e = transitionTeamTx(tx, transition, now)
		return e
	})
	return
}
func transitionTeamTx(tx *ateauthority.Tx, transition orchestration.TeamTransition, now time.Time) (orchestration.TeamSnapshot, error) {
	team, err := getRecord[orchestration.TeamSnapshot](tx, "team", string(transition.TeamID))
	if err != nil {
		return team, err
	}
	if transition.Expected != orchestration.AnySequence && transition.Expected != team.Version {
		return team, fmt.Errorf("%w: expected %d, current %d", orchestration.ErrTeamStateConflict, transition.Expected, team.Version)
	}
	if err := orchestration.ValidateTeamTransition(team.State, transition.Target); err != nil {
		return team, err
	}
	types := map[orchestration.TeamState]orchestration.EventType{
		orchestration.TeamActive: orchestration.EventTeamReopened, orchestration.TeamDraining: orchestration.EventTeamDraining,
		orchestration.TeamCanceling: orchestration.EventTeamCancelRequested, orchestration.TeamCompleted: orchestration.EventTeamCompleted,
		orchestration.TeamFailed: orchestration.EventTeamFailed, orchestration.TeamCanceled: orchestration.EventTeamCanceled,
	}
	eventType, ok := types[transition.Target]
	if !ok {
		return team, fmt.Errorf("%w: unsupported Team target %q", orchestration.ErrInvalidEvent, transition.Target)
	}
	_, err = appendEvents(tx, orchestration.Stream{Type: orchestration.StreamTeam, ID: string(team.ID), TeamID: team.ID}, orchestration.AnySequence,
		orchestration.PendingEvent{Type: eventType, ActorAgentID: transition.ActorAgentID, CorrelationID: transition.CorrelationID, Payload: orchestration.MustJSONPayload(map[string]any{"from": team.State, "to": transition.Target, "reason": transition.Reason})})
	if err != nil {
		return team, err
	}
	team.State, team.UpdatedAt = transition.Target, now
	team.Version++
	return team, putRecord(tx, "team", string(team.ID), team)
}
