package atebridge

import (
	"context"
	"errors"
	"testing"

	"github.com/vkviyu/agent-temporal-engine/agentprofile"
	"github.com/vkviyu/agent-temporal-engine/api"
)

func TestHasExecutionDoesNotCreateMissingOrPartialExecution(t *testing.T) {
	for _, drop := range []string{"before", "after"} {
		t.Run(drop, func(t *testing.T) {
			f := newFixture(t)
			ctx := context.Background()
			if found, err := f.b.HasExecution(ctx, "session", "turn"); err != nil || found {
				t.Fatalf("unprepared execution: %v %v", found, err)
			}
			if err := f.b.CompleteExecution(ctx, "session", "turn", false); err == nil {
				t.Fatal("unprepared completion accepted")
			}
			if len(f.p.commits) != 1 {
				t.Fatal("status check synthesized an execution")
			}
			f.p.check = func(request api.CommitRequest) error {
				if request.Method != api.MethodApplyAgent {
					return nil
				}
				command, err := agentprofile.DecodeCommand(request.Body)
				if err != nil {
					return err
				}
				if command.Control != nil && command.Control.GrantAuthority != nil {
					f.p.drop = drop
					f.p.check = nil
				}
				return nil
			}
			_, err := f.b.EnsureExecution(ctx, "session", "turn", "original goal")
			if !errors.Is(err, ErrOutcomeUnknown) {
				t.Fatalf("want ambiguous final preparation: %v", err)
			}
			before := len(f.p.commits)
			f.restart(t)
			found, err := f.b.HasExecution(ctx, "session", "turn")
			if err != nil || found != (drop == "after") {
				t.Fatalf("reconciled preparation: %v %v", found, err)
			}
			if len(f.p.commits) != before {
				t.Fatal("HasExecution retried a mutation")
			}
			if !found {
				if err := f.b.CompleteExecution(ctx, "session", "turn", false); err == nil {
					t.Fatal("partial preparation completed")
				}
				if len(f.p.commits) != before {
					t.Fatal("partial completion synthesized a terminal event")
				}
				if _, err := f.b.EnsureExecution(ctx, "session", "turn", "original goal"); !errors.Is(err, ErrOutcomeUnknown) {
					t.Fatal(err)
				}
				return
			}
			if err := f.b.CompleteExecution(ctx, "session", "turn", false); err != nil {
				t.Fatal(err)
			}
			if found, err := f.b.HasExecution(ctx, "session", "turn"); err != nil || !found {
				t.Fatalf("terminal execution no longer exists: %v %v", found, err)
			}
		})
	}
}
