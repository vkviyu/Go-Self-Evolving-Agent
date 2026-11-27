// Package observation carries best-effort execution diagnostics. The context
// selects the destination and correlation IDs for one execution tree.
package observation

import (
	"context"
	"maps"
	"time"
)

// Scope uses existing host IDs so events can be joined with durable history.
type Scope struct {
	SessionID     string `json:"session_id,omitempty"`
	TurnID        string `json:"turn_id,omitempty"`
	TeamID        string `json:"team_id,omitempty"`
	AgentID       string `json:"agent_id,omitempty"`
	ParentAgentID string `json:"parent_agent_id,omitempty"`
	RunID         string `json:"run_id,omitempty"`
	TaskID        string `json:"task_id,omitempty"`
	RequestID     string `json:"request_id,omitempty"`
	CallID        string `json:"call_id,omitempty"`
}

// Event contains identifiers, enum labels and counters. Producers must never
// supply user content, URLs, credentials, raw errors or arbitrary wire fields.
type Event struct {
	Scope
	Version   int               `json:"v"`
	Sequence  uint64            `json:"seq"`
	At        time.Time         `json:"at"`
	Component string            `json:"component"`
	Name      string            `json:"name"`
	Status    string            `json:"status,omitempty"`
	Metrics   map[string]int64  `json:"metrics,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
	// Dropped is a cumulative queue/validation loss count for this recorder.
	Dropped uint64 `json:"dropped,omitempty"`
}

type scopeKey struct{}
type recorderKey struct{}

// ScopeFrom returns the correlation IDs carried by ctx, or the zero scope when none are
// present.
func ScopeFrom(ctx context.Context) Scope {
	scope, _ := ctx.Value(scopeKey{}).(Scope)
	return scope
}

// WithScope overlays the supplied correlation IDs on the existing observation scope when
// recording is enabled.
func WithScope(ctx context.Context, scope Scope) context.Context {
	if !Enabled(ctx) {
		return ctx
	}
	return context.WithValue(ctx, scopeKey{}, ScopeFrom(ctx).overlay(scope))
}

// WithExactScope starts a new execution boundary. A detached worker keeps
// the recorder and cancellation lifetime without inheriting its caller's IDs.
func WithExactScope(ctx context.Context, scope Scope) context.Context {
	if !Enabled(ctx) {
		return ctx
	}
	return context.WithValue(ctx, scopeKey{}, scope)
}

func (s Scope) overlay(child Scope) Scope {
	if child.SessionID != "" {
		s.SessionID = child.SessionID
	}
	if child.TurnID != "" {
		s.TurnID = child.TurnID
	}
	if child.TeamID != "" {
		s.TeamID = child.TeamID
	}
	if child.AgentID != "" {
		s.AgentID = child.AgentID
	}
	if child.ParentAgentID != "" {
		s.ParentAgentID = child.ParentAgentID
	}
	if child.RunID != "" {
		s.RunID = child.RunID
	}
	if child.TaskID != "" {
		s.TaskID = child.TaskID
	}
	if child.RequestID != "" {
		s.RequestID = child.RequestID
	}
	if child.CallID != "" {
		s.CallID = child.CallID
	}
	return s
}

func recorderFrom(ctx context.Context) *Recorder {
	if ctx == nil {
		return nil
	}
	r, _ := ctx.Value(recorderKey{}).(*Recorder)
	return r
}

// Enabled reports whether the context has a recorder that is still accepting observations.
func Enabled(ctx context.Context) bool {
	r := recorderFrom(ctx)
	return r != nil && !r.stopped.Load()
}

// Emit takes a bounded copy and never waits for an exporter or queue space.
func Emit(ctx context.Context, e Event) {
	r := recorderFrom(ctx)
	if r == nil || r.stopped.Load() {
		return
	}
	e.Scope = ScopeFrom(ctx).overlay(e.Scope)
	if !e.valid() {
		r.dropped.Add(1)
		return
	}
	e.At = time.Now().UTC()
	e.Metrics = maps.Clone(e.Metrics)
	e.Labels = maps.Clone(e.Labels)
	r.offer(e)
}

func (e Event) valid() bool {
	if e.Component == "" || e.Name == "" || len(e.Metrics) > 32 || len(e.Labels) > 8 {
		return false
	}
	for _, s := range []string{e.SessionID, e.TurnID, e.TeamID, e.AgentID, e.ParentAgentID, e.RunID, e.TaskID, e.RequestID, e.CallID, e.Component, e.Name, e.Status} {
		if len(s) > 256 {
			return false
		}
	}
	for k := range e.Metrics {
		if len(k) > 80 {
			return false
		}
	}
	for k, v := range e.Labels {
		if len(k) > 80 || len(v) > 128 {
			return false
		}
	}
	return true
}
