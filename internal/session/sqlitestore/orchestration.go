package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

type OrchestrationStore struct {
	db *sql.DB
}

func (store *OrchestrationStore) ListTeams(ctx context.Context) ([]orchestration.TeamID, error) {
	rows, err := store.db.QueryContext(ctx, `SELECT team_id FROM teams ORDER BY created_at ASC, team_id ASC`)
	if err != nil {
		return nil, fmt.Errorf("list orchestration Teams: %w", err)
	}
	defer rows.Close()
	teams := make([]orchestration.TeamID, 0)
	for rows.Next() {
		var teamID string
		if err := rows.Scan(&teamID); err != nil {
			return nil, fmt.Errorf("scan orchestration Team: %w", err)
		}
		teams = append(teams, orchestration.TeamID(teamID))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate orchestration Teams: %w", err)
	}
	return teams, nil
}

func (store *OrchestrationStore) EnsureRootThread(ctx context.Context, seed orchestration.RootThreadSeed) (root orchestration.RootThread, created bool, err error) {
	if seed.SessionID == "" {
		return orchestration.RootThread{}, false, fmt.Errorf("%w: session ID is required", orchestration.ErrInvalidEvent)
	}
	if seed.ProfileRevision == "" {
		seed.ProfileRevision = "default-v1"
	}
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return orchestration.RootThread{}, false, fmt.Errorf("begin root thread projection: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if existing, found, queryErr := rootThreadBySession(ctx, tx, seed.SessionID); queryErr != nil {
		return orchestration.RootThread{}, false, queryErr
	} else if found {
		// A resumed session starts a new run of its Team.
		if existing.TeamState, err = reopenTeam(ctx, tx, existing.TeamID); err != nil {
			return orchestration.RootThread{}, false, fmt.Errorf("reopen root team: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return orchestration.RootThread{}, false, fmt.Errorf("commit root thread lookup: %w", err)
		}
		return existing, false, nil
	}
	if seed.TeamID == "" {
		seed.TeamID, err = orchestration.NewTeamID()
		if err != nil {
			return orchestration.RootThread{}, false, err
		}
	}
	if seed.AgentID == "" {
		seed.AgentID, err = orchestration.NewAgentID()
		if err != nil {
			return orchestration.RootThread{}, false, err
		}
	}
	if seed.ThreadID == "" {
		seed.ThreadID, err = orchestration.NewThreadID()
		if err != nil {
			return orchestration.RootThread{}, false, err
		}
	}
	if err := seed.TeamID.Validate(); err != nil {
		return orchestration.RootThread{}, false, err
	}
	if err := seed.AgentID.Validate(); err != nil {
		return orchestration.RootThread{}, false, err
	}
	if err := seed.ThreadID.Validate(); err != nil {
		return orchestration.RootThread{}, false, err
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `INSERT INTO teams(team_id, root_agent_id, state, version, created_at, updated_at) VALUES(?, ?, ?, ?, ?, ?)`,
		string(seed.TeamID), string(seed.AgentID), orchestration.TeamActive, 1, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		return orchestration.RootThread{}, false, fmt.Errorf("insert root team: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO agents(agent_id, team_id, parent_agent_id, thread_id, state, profile_revision, profile_snapshot, version, created_at, updated_at) VALUES(?, ?, '', ?, ?, ?, ?, ?, ?, ?)`,
		string(seed.AgentID), string(seed.TeamID), string(seed.ThreadID), orchestration.AgentQueued, seed.ProfileRevision, []byte(nonNilJSON(seed.ProfileSnapshot)), 1, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		return orchestration.RootThread{}, false, fmt.Errorf("insert root agent: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_threads(thread_id, session_id, team_id, agent_id, context_policy, profile_revision, version, created_at, updated_at) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(seed.ThreadID), string(seed.SessionID), string(seed.TeamID), string(seed.AgentID), "none", seed.ProfileRevision, 1, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		return orchestration.RootThread{}, false, fmt.Errorf("insert root thread: %w", err)
	}
	if _, err := appendOrchestrationEvents(ctx, tx, orchestration.Stream{Type: orchestration.StreamTeam, ID: string(seed.TeamID), TeamID: seed.TeamID}, orchestration.AnySequence,
		orchestration.PendingEvent{Type: orchestration.EventTeamCreated, Payload: orchestration.MustJSONPayload(map[string]any{"root_agent_id": seed.AgentID, "source": "legacy_session"})},
	); err != nil {
		return orchestration.RootThread{}, false, fmt.Errorf("record root team event: %w", err)
	}
	if _, err := appendOrchestrationEvents(ctx, tx, orchestration.Stream{Type: orchestration.StreamAgent, ID: string(seed.AgentID), TeamID: seed.TeamID}, orchestration.AnySequence,
		orchestration.PendingEvent{Type: orchestration.EventAgentCreated, Payload: orchestration.MustJSONPayload(map[string]any{"thread_id": seed.ThreadID, "profile_revision": seed.ProfileRevision, "profile_snapshot": seed.ProfileSnapshot})},
		orchestration.PendingEvent{Type: orchestration.EventAgentQueued, ActorAgentID: seed.AgentID, Payload: orchestration.MustJSONPayload(map[string]any{"reason": "root_thread_ready"})},
	); err != nil {
		return orchestration.RootThread{}, false, fmt.Errorf("record root agent events: %w", err)
	}
	if _, err := appendOrchestrationEvents(ctx, tx, orchestration.Stream{Type: orchestration.StreamThread, ID: string(seed.ThreadID), TeamID: seed.TeamID}, orchestration.AnySequence,
		orchestration.PendingEvent{Type: orchestration.EventThreadCreated, Payload: orchestration.MustJSONPayload(map[string]any{"session_id": seed.SessionID, "agent_id": seed.AgentID})},
	); err != nil {
		return orchestration.RootThread{}, false, fmt.Errorf("record root thread event: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return orchestration.RootThread{}, false, fmt.Errorf("commit root thread projection: %w", err)
	}
	return orchestration.RootThread{
		TeamID: seed.TeamID, AgentID: seed.AgentID, ThreadID: seed.ThreadID, SessionID: seed.SessionID,
		TeamState: orchestration.TeamActive, AgentState: orchestration.AgentQueued,
		ProfileRevision: seed.ProfileRevision, CreatedAt: now,
	}, true, nil
}

func (store *OrchestrationStore) RootThreadBySession(ctx context.Context, sessionID session.ID) (orchestration.RootThread, error) {
	if sessionID == "" {
		return orchestration.RootThread{}, fmt.Errorf("%w: session ID is required", orchestration.ErrInvalidEvent)
	}
	root, found, err := rootThreadBySession(ctx, store.db, sessionID)
	if err != nil {
		return orchestration.RootThread{}, err
	}
	if !found {
		return orchestration.RootThread{}, fmt.Errorf("%w: root thread for session %q", orchestration.ErrNotFound, sessionID)
	}
	return root, nil
}

func rootThreadBySession(ctx context.Context, query rowQuery, sessionID session.ID) (orchestration.RootThread, bool, error) {
	var root orchestration.RootThread
	var teamID, agentID, threadID, teamState, agentState, profileRevision, createdAt string
	err := query.QueryRowContext(ctx, `
SELECT t.team_id, t.root_agent_id, th.thread_id, t.state, a.state,
       a.profile_revision, th.created_at
FROM agent_threads th
JOIN teams t ON t.team_id = th.team_id
JOIN agents a ON a.agent_id = th.agent_id
WHERE th.session_id = ? AND a.parent_agent_id = ''`, string(sessionID)).Scan(
		&teamID, &agentID, &threadID, &teamState, &agentState, &profileRevision, &createdAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return orchestration.RootThread{}, false, nil
	}
	if err != nil {
		return orchestration.RootThread{}, false, fmt.Errorf("load root thread: %w", err)
	}
	root.TeamID = orchestration.TeamID(teamID)
	root.AgentID = orchestration.AgentID(agentID)
	root.ThreadID = orchestration.ThreadID(threadID)
	root.SessionID = sessionID
	root.TeamState = orchestration.TeamState(teamState)
	root.AgentState = orchestration.AgentState(agentState)
	root.ProfileRevision = profileRevision
	root.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return orchestration.RootThread{}, false, fmt.Errorf("parse root thread time: %w", err)
	}
	return root, true, nil
}
