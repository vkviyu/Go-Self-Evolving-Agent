package orchestration

import (
	"context"
	"errors"
	"time"
)

var (
	ErrTeamStateConflict = errors.New("Team state version conflict")
	// ErrTeamNotActive rejects new Agents while a Team is not active.
	ErrTeamNotActive = errors.New("Team is not active")
)

type (
	// TeamSnapshot is the durable projection of one Team.
	TeamSnapshot struct {
		ID          TeamID    `json:"team_id"`
		RootAgentID AgentID   `json:"root_agent_id"`
		State       TeamState `json:"state"`
		Version     uint64    `json:"version"`
		UpdatedAt   time.Time `json:"updated_at"`
	}

	// TeamTransition moves a Team to Target when its version is Expected.
	TeamTransition struct {
		TeamID        TeamID
		Expected      uint64
		Target        TeamState
		ActorAgentID  AgentID
		CorrelationID string
		Reason        string
	}

	// TeamRepository reads and transitions Teams. Each transition records the
	// matching team.* event in the same transaction.
	TeamRepository interface {
		// GetTeam returns the persisted team snapshot, including its lifecycle state and
		// version.
		GetTeam(ctx context.Context, teamID TeamID) (TeamSnapshot, error)
		// TransitionTeam applies a team lifecycle change against the recorded version.
		TransitionTeam(ctx context.Context, transition TeamTransition) (TeamSnapshot, error)
	}
)
