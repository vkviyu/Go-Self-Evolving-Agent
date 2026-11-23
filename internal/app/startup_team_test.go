package app

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/aterequest"
	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	sessionate "github.com/vkviyu/go-self-evolving-agent/internal/session/atestore"
)

func TestAuthorityStartupRecoversOwnedChildBeforeSupervisor(t *testing.T) {
	for _, mode := range []string{"chat", "control"} {
		t.Run(mode, func(t *testing.T) {
			ctx := t.Context()
			cfg := testConfig(t, config.Config{DataDirectory: t.TempDir(), WorkspaceDirectory: t.TempDir()})
			first, err := testOpen(t, ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = first.Close() })
			root, _, err := first.EnsureRootThread(ctx, "recover-team")
			if err != nil {
				t.Fatal(err)
			}
			child, err := first.repositories.CreateChild(ctx, orchestration.SpawnSpec{
				TeamID: root.TeamID, ParentAgentID: root.AgentID, ProfileRevision: "worker-v1",
				InitialPrompt: "recover original evidence", IdempotencyKey: "pending-child",
			})
			if err != nil {
				t.Fatal(err)
			}
			pending := &startupPending{ATERequestStore: first.database.ATERequests(), mode: "unattempted"}
			writer, err := sessionate.New(sessionate.Options{
				Process: first.authority.bound, Pending: pending, Space: authoritySpace, StoreID: first.authority.storeID,
			})
			if err != nil {
				t.Fatal(err)
			}
			sequence, err := writer.CurrentSequence(ctx, child.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			input := session.PendingEvent{Type: session.EventUserMessage, CorrelationID: "child-turn",
				Payload: json.RawMessage(`{"text":"` + strings.Repeat("x", 400<<10) + `"}`)}
			if _, err := writer.Append(ctx, child.SessionID, sequence, input); err == nil {
				t.Fatal("fault did not interrupt child upload")
			}
			if err := first.Close(); err != nil {
				t.Fatal(err)
			}
			var second *App
			if mode == "chat" {
				second, err = testOpen(t, ctx, cfg)
			} else {
				second, err = OpenControl(ctx, cfg)
			}
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = second.Close() })
			if mode == "chat" {
				_, _, err = second.EnsureRootThread(ctx, root.SessionID)
			} else if err = second.AttachExistingTeam(ctx, root.TeamID); err == nil {
				// Stop before Supervisor execution: only request reconciliation
				// and resource preparation may advance the child's history.
				err = second.prepareControlSupervisor(ctx, root.TeamID)
			}
			if err != nil {
				t.Fatalf("owned child plan blocked %s startup: %v", mode, err)
			}
			history, err := second.authority.sessions.Load(ctx, child.SessionID, sequence, 10)
			if err != nil || len(history) != 1 || !reflect.DeepEqual(history[0], pending.original) {
				t.Fatalf("original child evidence changed: count=%d err=%v", len(history), err)
			}
			left, err := second.database.ATERequests().List(ctx, aterequest.ListOptions{Limit: 128, MaxBytes: aterequest.MaxListBytes})
			if err != nil || len(left) != 0 || second.sessionOwners[child.SessionID] == nil {
				t.Fatalf("child plan not reconciled under ownership: pending=%d err=%v", len(left), err)
			}
		})
	}
}
