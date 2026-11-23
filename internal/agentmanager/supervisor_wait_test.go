package agentmanager

import (
	"context"
	"testing"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

type completingWaitRepository struct {
	orchestration.AgentRepository
	state orchestration.AgentState
	wake  func()
}

func (repository *completingWaitRepository) GetAgent(_ context.Context, id orchestration.AgentID) (orchestration.AgentSnapshot, error) {
	state := repository.state
	if state == orchestration.AgentRunning {
		repository.state = orchestration.AgentCompleted
		repository.wake()
	}
	return orchestration.AgentSnapshot{ID: id, State: state}, nil
}

func TestWaitObservesCompletionDuringStateLookup(t *testing.T) {
	repository := &completingWaitRepository{state: orchestration.AgentRunning}
	supervisor := &Supervisor{agents: repository, wake: make(chan struct{})}
	repository.wake = supervisor.signal
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := supervisor.Wait(ctx, WaitCommand{AgentIDs: []orchestration.AgentID{"child"}})
	if err != nil || !result.Satisfied || len(result.Agents) != 1 || result.Agents[0].State != orchestration.AgentCompleted {
		t.Fatalf("completion during wait lookup was lost: result=%+v err=%v", result, err)
	}
}
