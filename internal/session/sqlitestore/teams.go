package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

// GetTeam loads the durable state of a Team.
func (store *OrchestrationStore) GetTeam(ctx context.Context, teamID orchestration.TeamID) (orchestration.TeamSnapshot, error) {
	if err := teamID.Validate(); err != nil {
		return orchestration.TeamSnapshot{}, fmt.Errorf("invalid Team ID: %w", err)
	}
	return teamByID(ctx, store.db, teamID)
}

// TransitionTeam moves a Team to a new state and records its team.* event.
func (store *OrchestrationStore) TransitionTeam(ctx context.Context, transition orchestration.TeamTransition) (orchestration.TeamSnapshot, error) {
	if err := transition.TeamID.Validate(); err != nil {
		return orchestration.TeamSnapshot{}, fmt.Errorf("invalid Team transition ID: %w", err)
	}
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return orchestration.TeamSnapshot{}, fmt.Errorf("begin Team transition: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	updated, err := transitionTeam(ctx, tx, transition)
	if err != nil {
		return orchestration.TeamSnapshot{}, err
	}
	if err := tx.Commit(); err != nil {
		return orchestration.TeamSnapshot{}, fmt.Errorf("commit Team transition: %w", err)
	}
	return updated, nil
}

// transitionTeam applies a version-guarded Team transition inside tx.
func transitionTeam(ctx context.Context, tx *sql.Tx, transition orchestration.TeamTransition) (orchestration.TeamSnapshot, error) {
	team, err := teamByID(ctx, tx, transition.TeamID)
	if err != nil {
		return orchestration.TeamSnapshot{}, err
	}
	if transition.Expected != orchestration.AnySequence && transition.Expected != team.Version {
		return orchestration.TeamSnapshot{}, fmt.Errorf("%w: expected %d, current %d", orchestration.ErrTeamStateConflict, transition.Expected, team.Version)
	}
	if err := orchestration.ValidateTeamTransition(team.State, transition.Target); err != nil {
		return orchestration.TeamSnapshot{}, err
	}
	eventType, err := teamTransitionEvent(transition.Target)
	if err != nil {
		return orchestration.TeamSnapshot{}, err
	}
	now := time.Now().UTC()
	if err := execCAS(ctx, tx, orchestration.ErrTeamStateConflict, `UPDATE teams SET state = ?, version = version + 1, updated_at = ? WHERE team_id = ? AND version = ?`,
		string(transition.Target), now.Format(time.RFC3339Nano), string(team.ID), team.Version); err != nil {
		return orchestration.TeamSnapshot{}, fmt.Errorf("update Team state: %w", err)
	}
	streamSequence, err := currentOrchestrationSequence(ctx, tx, orchestration.StreamTeam, string(team.ID))
	if err != nil {
		return orchestration.TeamSnapshot{}, err
	}
	if _, err := appendOrchestrationEvents(ctx, tx, orchestration.Stream{Type: orchestration.StreamTeam, ID: string(team.ID), TeamID: team.ID}, streamSequence,
		orchestration.PendingEvent{Type: eventType, ActorAgentID: transition.ActorAgentID, CorrelationID: transition.CorrelationID, Payload: orchestration.MustJSONPayload(map[string]any{
			"from": team.State, "to": transition.Target, "reason": transition.Reason,
		})},
	); err != nil {
		return orchestration.TeamSnapshot{}, fmt.Errorf("record Team transition: %w", err)
	}
	return teamByID(ctx, tx, team.ID)
}

// teamTransitionEvent names the event recorded when a Team enters state.
func teamTransitionEvent(state orchestration.TeamState) (orchestration.EventType, error) {
	switch state {
	case orchestration.TeamActive:
		return orchestration.EventTeamReopened, nil
	case orchestration.TeamDraining:
		return orchestration.EventTeamDraining, nil
	case orchestration.TeamCanceling:
		return orchestration.EventTeamCancelRequested, nil
	case orchestration.TeamCompleted:
		return orchestration.EventTeamCompleted, nil
	case orchestration.TeamFailed:
		return orchestration.EventTeamFailed, nil
	case orchestration.TeamCanceled:
		return orchestration.EventTeamCanceled, nil
	default:
		return "", fmt.Errorf("%w: unsupported Team target %q", orchestration.ErrInvalidEvent, state)
	}
}

// reopenTeam makes a finished Team active again for a resumed session. Active,
// draining and canceling Teams keep their state.
func reopenTeam(ctx context.Context, tx *sql.Tx, teamID orchestration.TeamID) (orchestration.TeamState, error) {
	team, err := teamByID(ctx, tx, teamID)
	if err != nil {
		return "", err
	}
	switch team.State {
	case orchestration.TeamCompleted, orchestration.TeamFailed, orchestration.TeamCanceled:
		reopened, err := transitionTeam(ctx, tx, orchestration.TeamTransition{TeamID: teamID, Expected: team.Version, Target: orchestration.TeamActive, Reason: "session reopened"})
		return reopened.State, err
	default:
		return team.State, nil
	}
}

// requireActiveTeam rejects work for a Team that no longer accepts Agents.
func requireActiveTeam(ctx context.Context, query rowQuery, teamID orchestration.TeamID) error {
	team, err := teamByID(ctx, query, teamID)
	if err != nil {
		return err
	}
	if team.State != orchestration.TeamActive {
		return fmt.Errorf("%w: Team %s is %s", orchestration.ErrTeamNotActive, teamID, team.State)
	}
	return nil
}

// teamByID loads one Team row.
func teamByID(ctx context.Context, query rowQuery, teamID orchestration.TeamID) (orchestration.TeamSnapshot, error) {
	var id, rootAgentID, state, updatedAt string
	var version uint64
	err := query.QueryRowContext(ctx, `SELECT team_id, root_agent_id, state, version, updated_at FROM teams WHERE team_id = ?`, string(teamID)).
		Scan(&id, &rootAgentID, &state, &version, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return orchestration.TeamSnapshot{}, fmt.Errorf("%w: Team %s", orchestration.ErrNotFound, teamID)
	}
	if err != nil {
		return orchestration.TeamSnapshot{}, fmt.Errorf("load Team: %w", err)
	}
	updated, err := time.Parse(time.RFC3339Nano, updatedAt)
	if err != nil {
		return orchestration.TeamSnapshot{}, fmt.Errorf("parse Team time: %w", err)
	}
	return orchestration.TeamSnapshot{
		ID: orchestration.TeamID(id), RootAgentID: orchestration.AgentID(rootAgentID),
		State: orchestration.TeamState(state), Version: version, UpdatedAt: updated,
	}, nil
}
