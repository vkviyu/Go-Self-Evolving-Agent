package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/atebridge"
	"github.com/vkviyu/go-self-evolving-agent/internal/ateports"
	"github.com/vkviyu/go-self-evolving-agent/internal/experience"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	sessionate "github.com/vkviyu/go-self-evolving-agent/internal/session/atestore"
)

type (
	temporalLifecycle struct {
		bridge *atebridge.Bridge
	}
	temporalExperience struct {
		*experience.Service
		bridge *atebridge.Bridge
	}
)

// Sync reconciles unresolved native commands at runtime boundaries. Session
// events already live in ATE and require no source delivery.
func (t temporalExperience) Sync(ctx context.Context) error {
	return t.bridge.RecoverPending(ctx)
}

// MemoryBrowser exposes policy-filtered memory discovery without granting
// callers automatic adoption or mutation authority.
func (a *App) MemoryBrowser() (experience.MemoryBrowser, error) {
	if a == nil || a.experience == nil {
		return nil, errors.New("memory browser is unavailable")
	}
	return a.experience, nil
}

// openTemporal connects semantic experience to the same authoritative history
// used by the Agent Loop. Local database paths never define source identities.
func (a *App) openTemporal(ctx context.Context) error {
	if a.authority == nil {
		return errors.New("ATE authority must be initialized before experience")
	}
	state := authorityState{records: a.authority.records}
	bridge, err := atebridge.New(atebridge.Options{
		Process: a.authority.bound, Pending: a.database.ATERequests(),
		State: state, Namespace: "gsea/v1",
	})
	if err != nil {
		return err
	}
	if err := bridge.WithClient(ctx, func(*client.Client) error { return nil }); err != nil {
		return fmt.Errorf("start native ATE infrastructure (configure ate.binary or install ate): %w", err)
	}
	service, err := experience.New(experience.Options{Bridge: bridge, State: state, History: a.authority.sessions})
	if err != nil {
		return err
	}
	a.temporalBridge, a.experience = bridge, service
	a.events = temporalEvents{Store: a.authority.sessions, app: a}
	return nil
}

// temporalEvents adds host-scoped maintenance discovery to native history.
// Embedding the store preserves its append, correlation and history contracts.
// No pending-event projection is kept in the local runtime database.
type temporalEvents struct {
	*sessionate.Store
	app *App
}

var _ agentruntime.PendingExperienceMaintenanceStore = temporalEvents{}

func (s temporalEvents) PendingExperienceMaintenance(ctx context.Context, limit int) ([]session.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = ateports.DefaultLimit
	}
	limit = min(limit, ateports.MaxLimit)
	// Shared authority visibility does not grant execution ownership. Include
	// inactive children of owned Teams, but never sweep another App's Teams.
	s.app.stateMu.Lock()
	if s.app.closed {
		s.app.stateMu.Unlock()
		return nil, errors.New("application is closed")
	}
	ids := make(map[session.ID]struct{}, len(s.app.sessionOwners))
	for id := range s.app.sessionOwners {
		ids[id] = struct{}{}
	}
	teams := make([]orchestration.TeamID, 0, len(s.app.ownedTeams))
	for team := range s.app.ownedTeams {
		teams = append(teams, team)
	}
	repositories := s.app.repositories
	s.app.stateMu.Unlock()
	slices.Sort(teams)
	for _, team := range teams {
		agents, err := repositories.ListAgents(ctx, orchestration.AgentQuery{TeamID: team})
		if err != nil {
			return nil, err
		}
		for _, agent := range agents {
			if agent.TeamID != team || agent.SessionID == "" {
				return nil, sessionate.ErrCorrupt
			}
			ids[agent.SessionID] = struct{}{}
		}
	}
	ordered := make([]session.ID, 0, len(ids))
	for id := range ids {
		ordered = append(ordered, id)
	}
	slices.Sort(ordered)
	type candidate struct {
		event session.Event
		due   time.Time
	}
	pending := make([]candidate, 0)
	retainedBytes := 0
	for _, id := range ordered {
		through, err := s.HistoryCut(ctx, string(id))
		if err != nil {
			return nil, err
		}
		latest := make(map[string]session.Event)
		for after := uint64(0); after < through; {
			// Bound each read and retained status bytes; caller cancellation
			// bounds total work without rejecting older, longer histories.
			page, err := s.HistoryThrough(ctx, string(id), after, through, api.MaxSessionPageItems)
			if err != nil {
				return nil, err
			}
			if len(page) == 0 {
				return nil, sessionate.ErrCorrupt
			}
			for _, record := range page {
				after = record.Sequence
				if record.Kind != string(agentruntime.EventExperienceMaintenance) {
					continue
				}
				var event session.Event
				if err := json.Unmarshal(record.Payload, &event); err != nil {
					return nil, err
				}
				if event.SessionID != id || event.Sequence != record.Sequence || event.CorrelationID == "" ||
					event.Type != agentruntime.EventExperienceMaintenance {
					return nil, sessionate.ErrCorrupt
				}
				retainedBytes += len(event.Payload) - len(latest[event.CorrelationID].Payload)
				if retainedBytes > sessionate.MaxReadBytes {
					return nil, sessionate.ErrLimit
				}
				latest[event.CorrelationID] = event
			}
		}
		for _, event := range latest {
			var status agentruntime.ExperienceMaintenance
			if err := json.Unmarshal(event.Payload, &status); err != nil {
				return nil, err
			}
			if status.Status != "pending" && status.Status != "completed" {
				return nil, sessionate.ErrCorrupt
			}
			if status.Status == "pending" {
				var due time.Time
				if status.RetryAfter != nil {
					due = *status.RetryAfter
				}
				pending = append(pending, candidate{event: event, due: due})
			} else {
				retainedBytes -= len(event.Payload)
			}
		}
	}
	// Apply the result limit only after latest-state reduction and due ordering.
	// A completed turn or future admission must not hide an eligible job.
	slices.SortFunc(pending, func(a, b candidate) int {
		if order := a.due.Compare(b.due); order != 0 {
			return order
		}
		if a.event.SessionID != b.event.SessionID {
			return strings.Compare(string(a.event.SessionID), string(b.event.SessionID))
		}
		return strings.Compare(a.event.CorrelationID, b.event.CorrelationID)
	})
	result := make([]session.Event, 0, min(limit, len(pending)))
	for _, item := range pending[:min(limit, len(pending))] {
		result = append(result, item.event)
	}
	return result, ctx.Err()
}

// Finish records the native outcome after the runtime commits its source facts.
// A recoverable interruption keeps its execution open for exact turn replay.
func (l temporalLifecycle) Finish(ctx context.Context, sessionID, turnID string, outcome agentruntime.ExperienceOutcome) error {
	if outcome.Recoverable {
		return nil
	}
	ensured, err := l.bridge.HasExecution(ctx, sessionID, turnID)
	if err != nil || !ensured {
		return err
	}
	return l.bridge.CompleteExecution(ctx, sessionID, turnID, outcome.Succeeded)
}
