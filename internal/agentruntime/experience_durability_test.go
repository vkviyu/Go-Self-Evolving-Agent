package agentruntime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/memorystore"
)

type maintenanceBudgetStore struct {
	session.EventStore
	discoveryBudget time.Duration
	claimBudget     time.Duration
	appendBudget    time.Duration
	correlatedReads int
}

func (s *maintenanceBudgetStore) PendingExperienceMaintenance(ctx context.Context, _ int) ([]session.Event, error) {
	deadline, _ := ctx.Deadline()
	s.discoveryBudget = time.Until(deadline)
	return s.EventStore.Load(ctx, "origin", 0, 10)
}

func (s *maintenanceBudgetStore) CurrentSequence(ctx context.Context, id session.ID) (uint64, error) {
	deadline, _ := ctx.Deadline()
	s.claimBudget = time.Until(deadline)
	return s.EventStore.CurrentSequence(ctx, id)
}

func (s *maintenanceBudgetStore) Load(context.Context, session.ID, uint64, int) ([]session.Event, error) {
	return nil, errors.New("maintenance claim unexpectedly scanned unrelated history")
}

func (s *maintenanceBudgetStore) LoadByCorrelation(ctx context.Context, id session.ID, correlation string) ([]session.Event, error) {
	s.correlatedReads++
	all, err := s.EventStore.Load(ctx, id, 0, 100)
	result := make([]session.Event, 0)
	for _, event := range all {
		if event.CorrelationID == correlation {
			result = append(result, event)
		}
	}
	return result, err
}

func (s *maintenanceBudgetStore) Append(ctx context.Context, id session.ID, expected uint64, pending ...session.PendingEvent) ([]session.Event, error) {
	deadline, _ := ctx.Deadline()
	s.appendBudget = time.Until(deadline)
	return s.EventStore.Append(ctx, id, expected, pending...)
}

func TestExperienceRetryClaimHasIndependentDurabilityBudget(t *testing.T) {
	events := memorystore.New()
	seedExperienceMaintenance(t, events, "origin", "old")
	store := &maintenanceBudgetStore{EventStore: events}
	m := &recordingModel{
		events: store, sessionID: "current", turnID: "new",
		factory: &Factory{config: Config{ExperienceTimeout: time.Minute}},
	}
	if err := m.retryExperienceMaintenance(t.Context()); err != nil {
		t.Fatal(err)
	}
	if m.experienceRetry == nil || m.experienceRetry.sessionID != "origin" || store.correlatedReads != 1 {
		t.Fatalf("indexed native claim not admitted: job=%+v reads=%d", m.experienceRetry, store.correlatedReads)
	}
	if store.discoveryBudget <= 0 || store.discoveryBudget > experienceRetryBudget {
		t.Fatalf("discovery budget is unbounded: %v", store.discoveryBudget)
	}
	if store.claimBudget <= experienceRetryBudget || store.appendBudget <= experienceRetryBudget {
		t.Fatalf("claim reused discovery deadline: read=%v append=%v", store.claimBudget, store.appendBudget)
	}
}

func TestExperienceRetryCompletionUsesNormalDurabilityAfterCancellation(t *testing.T) {
	events := memorystore.New()
	store := &maintenanceBudgetStore{EventStore: events}
	m := &recordingModel{events: store, sessionID: "origin", turnID: "old"}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	status := &ExperienceMaintenance{Status: "completed", RetryAttempts: 1}
	if err := m.recordExperienceMaintenance(ctx, status); err != nil {
		t.Fatal(err)
	}
	if store.appendBudget <= time.Second {
		t.Fatalf("retry completion received diagnostic budget: %v", store.appendBudget)
	}
	actual := latestExperienceMaintenance(t, events, "origin", "old")
	if actual.Status != "completed" || actual.RetryAttempts != 1 {
		t.Fatalf("completion did not survive caller cancellation: %+v", actual)
	}
}
