package atebridge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/vkviyu/agent-temporal-engine/agentprofile"
	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/go-self-evolving-agent/internal/ateports"
)

func TestNativeExecutionLifecycleWithoutLocalMetadata(t *testing.T) {
	for _, success := range []bool{true, false} {
		t.Run(fmt.Sprint(success), func(t *testing.T) {
			f := newFixture(t)
			f.useNative(t)
			goal := strings.Repeat("complete native goal ", 100)
			if found, err := f.b.HasExecution(t.Context(), "session", "turn"); err != nil || found {
				t.Fatalf("missing execution was created: %v", err)
			}
			key, err := f.b.EnsureExecution(t.Context(), "session", "turn", goal)
			if err != nil {
				t.Fatal(err)
			}
			if f.b.state == nil || len(f.pendingRequests(t)) != 0 {
				t.Fatal("native preparation retained local metadata or completed bodies")
			}
			f.restart(t)
			f.useNative(t)
			count := len(f.p.commits)
			again, err := f.b.EnsureExecution(t.Context(), "session", "turn", goal)
			if err != nil || again != key || len(f.p.commits) != count {
				t.Fatalf("native replay changed execution: %v", err)
			}
			if _, err := f.b.EnsureExecution(t.Context(), "session", "turn", goal+"changed"); !errors.Is(err, ateports.ErrConflict) {
				t.Fatalf("native original goal was not pinned: %v", err)
			}
			if success {
				f.p.check = func(request api.CommitRequest) error {
					if request.Method == api.MethodApplyWork {
						f.p.drop = "after"
						f.p.check = nil
					}
					return nil
				}
				if err := f.b.CompleteExecution(t.Context(), "session", "turn", true); !errors.Is(err, ErrOutcomeUnknown) {
					t.Fatalf("checkpoint did not expose unknown result: %v", err)
				}
				f.restart(t)
				f.useNative(t)
				if err := f.b.RecoverPending(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.b.CompleteExecution(t.Context(), "session", "turn", success); err != nil {
				t.Fatal(err)
			}
			f.restart(t)
			f.useNative(t)
			count = len(f.p.commits)
			if err := f.b.CompleteExecution(t.Context(), "session", "turn", success); err != nil || len(f.p.commits) != count {
				t.Fatalf("completion rebuilt native commands: %v", err)
			}
			if err := f.b.CompleteExecution(t.Context(), "session", "turn", !success); !errors.Is(err, ateports.ErrConflict) {
				t.Fatalf("conflicting outcome replaced original: %v", err)
			}
			if len(f.pendingRequests(t)) != 0 {
				t.Fatal("completed lifecycle retained pending command bodies")
			}
			if err := f.b.WithClient(t.Context(), func(c *client.Client) error {
				state, err := c.ReadFullAgentState(t.Context(), api.ReadAgentStateRequest{Space: f.b.OperationalSpace(), Principal: "gsea"})
				if err != nil {
					return err
				}
				want := agentprofile.ExecutionCanceled
				if success {
					want = agentprofile.ExecutionCompleted
				}
				if len(state.Snapshot.Tasks) != 1 || len(state.Snapshot.Executions) != 1 ||
					len(state.Snapshot.Steps) != 1 || state.Snapshot.Executions[0].Status != want {
					t.Fatal("native execution semantics changed")
				}
				if success && (len(state.Snapshot.Actions) != 1 || state.Snapshot.Actions[0].Status != agentprofile.ActionApplied) {
					t.Fatal("native continuation left unresolved")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCompletionRecoversUnknownCheckpointWithoutInventedObservations(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	key, err := f.b.EnsureExecution(ctx, "session", "turn", "original goal")
	if err != nil {
		t.Fatal(err)
	}
	f.p.check = func(request api.CommitRequest) error {
		if request.Method == api.MethodApplyWork {
			f.p.drop = "after"
			f.p.check = nil
		}
		return nil
	}
	err = f.b.CompleteExecution(ctx, "session", "turn", true)
	if !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("want ambiguous work receipt: %v", err)
	}
	f.restart(t)
	if err := f.b.CompleteExecution(ctx, "session", "turn", true); err != nil {
		t.Fatal(err)
	}
	if err := f.b.WithClient(ctx, func(c *client.Client) error {
		known, err := c.GetKnowledgeState(ctx, api.ReadKnowledgeStateRequest{
			Space: f.b.OperationalSpace(), Principal: "gsea", Knowledge: &key,
		})
		if err != nil {
			return err
		}
		if len(known.Snapshot.Observations) != 0 {
			t.Fatal("completion invented observations")
		}
		state, err := c.ReadFullAgentState(ctx, api.ReadAgentStateRequest{Space: f.b.OperationalSpace(), Principal: "gsea"})
		if err != nil {
			return err
		}
		if len(state.Snapshot.Actions) != 1 || state.Snapshot.Actions[0].Status != agentprofile.ActionApplied {
			t.Fatal("checkpoint bookkeeping left an unresolved action")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
