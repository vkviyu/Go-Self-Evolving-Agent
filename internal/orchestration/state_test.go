package orchestration

import "testing"

func TestStateTransitions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		valid   func() error
		invalid func() error
	}{
		{"team", func() error { return ValidateTeamTransition(TeamCanceled, TeamActive) }, func() error { return ValidateTeamTransition(TeamActive, TeamCanceled) }},
		{"agent", func() error { return ValidateAgentTransition(AgentCompleted, AgentQueued) }, func() error { return ValidateAgentTransition(AgentCanceled, AgentQueued) }},
		{"task", func() error { return ValidateTaskTransition(TaskVerification, TaskCompleted) }, func() error { return ValidateTaskTransition(TaskCompleted, TaskReady) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if err := test.valid(); err != nil {
				t.Fatalf("valid transition failed: %v", err)
			}
			if err := test.invalid(); err == nil {
				t.Fatal("invalid transition succeeded")
			}
		})
	}
}

func TestRunningTaskCanPreserveNonSuccessfulAgentOutcomeAsBlocked(t *testing.T) {
	t.Parallel()
	for _, state := range []TaskState{TaskClaimed, TaskInProgress, TaskVerification} {
		if err := ValidateTaskTransition(state, TaskBlocked); err != nil {
			t.Fatalf("%s -> blocked: %v", state, err)
		}
	}
}
