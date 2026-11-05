package agentmanager

import (
	"context"
	"errors"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

// Only cancellation preparation is under test. Other repository methods must
// not be called: this boundary cannot repeat task effects or save the result.
type cancellationConflictRepository struct {
	orchestration.AgentRepository
	latest      orchestration.AgentSnapshot
	writeErr    error
	readErr     error
	transitions []orchestration.AgentTransition
	reads       int
}

func (r *cancellationConflictRepository) TransitionAgent(_ context.Context, transition orchestration.AgentTransition) (orchestration.AgentSnapshot, error) {
	r.transitions = append(r.transitions, transition)
	return orchestration.AgentSnapshot{}, r.writeErr
}

func (r *cancellationConflictRepository) GetAgent(context.Context, orchestration.AgentID) (orchestration.AgentSnapshot, error) {
	r.reads++
	return r.latest, r.readErr
}

func TestCancellationConflictAcceptsOnlySameRunCanceling(t *testing.T) {
	t.Parallel()
	current := orchestration.AgentSnapshot{ID: "agent-fixture", CurrentRunID: "run-original", State: orchestration.AgentRunning, Version: 2}
	fault := errors.New("repository unavailable")
	for _, test := range []struct {
		name     string
		state    orchestration.AgentState
		run      orchestration.RunID
		writeErr error
		readErr  error
		accept   bool
		reads    int
	}{
		{"same run canceling", orchestration.AgentCanceling, current.CurrentRunID, orchestration.ErrAgentStateConflict, nil, true, 1},
		{"new run canceling", orchestration.AgentCanceling, "run-replacement", orchestration.ErrAgentStateConflict, nil, false, 1},
		{"already canceled", orchestration.AgentCanceled, current.CurrentRunID, orchestration.ErrAgentStateConflict, nil, false, 1},
		{"already completed", orchestration.AgentCompleted, current.CurrentRunID, orchestration.ErrAgentStateConflict, nil, false, 1},
		{"resumed wait", orchestration.AgentQueued, current.CurrentRunID, orchestration.ErrAgentStateConflict, nil, false, 1},
		{"read failed", orchestration.AgentCanceling, current.CurrentRunID, orchestration.ErrAgentStateConflict, fault, false, 1},
		{"non-conflict failure", orchestration.AgentCanceling, current.CurrentRunID, fault, nil, false, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository := &cancellationConflictRepository{
				latest:   orchestration.AgentSnapshot{ID: current.ID, State: test.state, CurrentRunID: test.run, Version: 3},
				writeErr: test.writeErr, readErr: test.readErr,
			}
			supervisor := &Supervisor{agents: repository}
			got, err := supervisor.prepareAgentCancellation(context.Background(), current, "canceled")
			if test.accept {
				if err != nil || got.Version != repository.latest.Version || got.CurrentRunID != current.CurrentRunID || got.State != orchestration.AgentCanceling {
					t.Fatalf("same-run cancellation was not adopted: %+v, %v", got, err)
				}
			} else if !errors.Is(err, test.writeErr) {
				t.Fatalf("unrelated conflict accepted: %+v, %v", got, err)
			}
			if len(repository.transitions) != 1 || repository.reads != test.reads {
				t.Fatalf("unexpected retries: writes=%d reads=%d", len(repository.transitions), repository.reads)
			}
			transition := repository.transitions[0]
			if transition.Expected != current.Version || transition.Target != orchestration.AgentCanceling || transition.Result != nil || transition.AgentID != current.ID {
				t.Fatalf("preparation bypassed CAS or committed an outcome: %+v", transition)
			}
		})
	}
}
