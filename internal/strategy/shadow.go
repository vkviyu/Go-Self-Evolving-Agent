package strategy

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ShadowStore intentionally cannot publish or change trusted expectations.
type ShadowStore interface {
	Load(context.Context, string) (Spec, error)
	Suite(context.Context, string) (Suite, error)
	SaveReport(context.Context, Report) (string, error)
}

// Comparator is implemented by the existing Eino Factory, not a second loop.
type Comparator interface {
	CompareStrategy(context.Context, Spec, Suite) (Report, error)
}

// ShadowService owns serialized comparisons and durable evidence. The host
// binds the authorized root comparator and manages worker cancellation/join.
type ShadowService struct {
	store ShadowStore
	gate  chan struct{}
}

func NewShadowService(store ShadowStore) (*ShadowService, error) {
	if store == nil {
		return nil, errors.New("strategy shadow service requires a store")
	}
	return &ShadowService{store: store, gate: make(chan struct{}, 1)}, nil
}

func (service *ShadowService) Compare(ctx context.Context, comparator Comparator, candidateID, suiteID string) (Report, string, error) {
	if comparator == nil {
		return Report{}, "", errors.New("shadow comparison requires a configured model factory")
	}
	select {
	case service.gate <- struct{}{}:
	case <-ctx.Done():
		return Report{}, "", ctx.Err()
	}
	defer func() { <-service.gate }()
	spec, err := service.store.Load(ctx, candidateID)
	if err != nil {
		return Report{}, "", err
	}
	suite, err := service.store.Suite(ctx, suiteID)
	if err != nil {
		return Report{}, "", err
	}
	report, compareErr := comparator.CompareStrategy(ctx, spec, suite)
	if compareErr != nil {
		report.CandidateID, report.SuiteID = spec.ID(), Digest(suite)
		report.Failure = fmt.Sprintf("%.512s", compareErr.Error())
		report.Eligible, report.Reason = report.Acceptance()
		if report.EvaluatedAt.IsZero() {
			report.EvaluatedAt = time.Now().UTC()
		}
	}
	// A canceled/failed comparison is evidence too. Never silently rerun paid
	// work when a completed tool result cannot be durably recorded.
	durable, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	id, err := service.store.SaveReport(durable, report)
	if err != nil {
		err = errors.Join(ErrEvidence, err)
	}
	return report, id, errors.Join(compareErr, err)
}
