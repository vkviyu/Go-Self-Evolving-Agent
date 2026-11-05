package sqlitestore

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentflow"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentgoal"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/plugin"
	"github.com/vkviyu/go-self-evolving-agent/internal/retention"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

func TestRetentionArchivePreservesHistoryAndCannotResurrect(t *testing.T) {
	ctx := context.Background()
	database := abandonmentDatabase(t)
	manifest := runtimeManifest(t, "v1")
	id, _ := manifest.ID()
	ref := tool.SnapshotReference{Kind: "turn", Scope: "root", ID: "done"}
	if err := database.RuntimeSnapshots().Save(ctx, manifest, ref); err != nil {
		t.Fatal(err)
	}
	recordAbandonmentTurn(t, database, "root", "done", id, true)
	plan, err := database.RetentionPlan(ctx, "root")
	if err != nil || len(plan.Blockers) > 0 || len(plan.Consumers) != 1 {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	if _, err := database.CloseRetention(ctx, "root", plan.ID, false); err == nil {
		t.Fatal("accepted missing confirmation")
	}
	receipt, err := database.CloseRetention(ctx, "root", plan.ID, true)
	if err != nil || receipt.ReleasedReferences != 1 || receipt.EvidenceDeleted {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	replayed, err := database.CloseRetention(ctx, "root", plan.ID, true)
	if err != nil || !reflect.DeepEqual(receipt, replayed) {
		t.Fatalf("replay=%+v err=%v", replayed, err)
	}
	if err := session.CheckRecovery(ctx, database.SessionEvents(), "root", "done"); !errors.Is(err, session.ErrRecoveryArchived) {
		t.Fatalf("archived turn resumed: %v", err)
	}
	if err := database.RuntimeSnapshots().Save(ctx, manifest, ref); !errors.Is(err, tool.ErrSnapshotReferenceReleased) {
		t.Fatalf("reference revived: %v", err)
	}
	events, err := database.SessionEvents().LoadByCorrelation(ctx, "root", "done")
	if err != nil || len(events) != 3 || events[1].Type != session.EventTurnCompleted {
		t.Fatalf("history=%+v %v", events, err)
	}
	if _, err := database.RuntimeSnapshots().Load(ctx, id); err != nil {
		t.Fatal("manifest deleted", err)
	}
	if err := database.Compact(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestRetentionPreviewRevalidatesAndRollsBack(t *testing.T) {
	for _, mode := range []string{"changed", "audit-failure", "receipt-failure", "running-goal", "incomplete", "orphan-reference"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			database := abandonmentDatabase(t)
			manifest := runtimeManifest(t, "v1")
			id, _ := manifest.ID()
			ref := tool.SnapshotReference{Kind: "turn", Scope: "root", ID: "target"}
			if err := database.RuntimeSnapshots().Save(ctx, manifest, ref); err != nil {
				t.Fatal(err)
			}
			if mode != "orphan-reference" {
				recordAbandonmentTurn(t, database, "root", "target", id, mode != "incomplete")
			}
			if mode == "running-goal" {
				goal := createAbandonmentGoal(t, database)
				if _, err := database.Orchestration().StartRound(ctx, goal.ID, 1, "target", "input"); err != nil {
					t.Fatal(err)
				}
			}
			plan, err := database.RetentionPlan(ctx, "root")
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "changed":
				if _, err := database.SessionEvents().Append(ctx, "root", session.AnySequence, session.PendingEvent{Type: session.EventUserMessage, Payload: []byte(`{"content":"new input"}`)}); err != nil {
					t.Fatal(err)
				}
			case "audit-failure":
				if _, err := database.db.Exec(`CREATE TRIGGER fail_archive BEFORE INSERT ON session_events WHEN NEW.event_type='turn.archived' BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
					t.Fatal(err)
				}
			case "receipt-failure":
				if _, err := database.db.Exec(`CREATE TRIGGER fail_archive BEFORE INSERT ON maintenance_receipts BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
					t.Fatal(err)
				}
			}
			_, err = database.CloseRetention(ctx, "root", plan.ID, true)
			if err == nil {
				t.Fatal("accepted unsafe archive")
			}
			if mode == "changed" && !errors.Is(err, retention.ErrPlanChanged) {
				t.Fatalf("stale=%v", err)
			}
			if refs, err := database.RuntimeSnapshots().References(ctx, id); err != nil || len(refs) != 1 {
				t.Fatalf("lost ref: %+v %v", refs, err)
			}
			if err := session.CheckRecovery(ctx, database.SessionEvents(), "root", "target"); err != nil {
				t.Fatal("failure closed recovery", err)
			}
		})
	}
}

func TestRetentionCoversTerminalGoalsChildrenAndFlows(t *testing.T) {
	ctx := context.Background()
	database := abandonmentDatabase(t)
	root, _, err := database.Orchestration().EnsureRootThread(ctx, orchestration.RootThreadSeed{SessionID: "root"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := database.Orchestration().CreateChild(ctx, orchestration.SpawnSpec{TeamID: root.TeamID, ParentAgentID: root.AgentID, ProfileRevision: "worker-v1"})
	if err != nil {
		t.Fatal(err)
	}
	recordAbandonmentTurn(t, database, child.SessionID, "child-turn", "", true)
	plan, err := database.RetentionPlan(ctx, child.SessionID)
	if err != nil || len(plan.Blockers) == 0 {
		t.Fatalf("live child not held: %+v %v", plan, err)
	}
	if _, err := database.db.Exec(`UPDATE agents SET state='completed',result=? WHERE agent_id=?`, []byte(`{"status":"succeeded","summary":"done"}`), string(child.ID)); err != nil {
		t.Fatal(err)
	}
	plan, err = database.RetentionPlan(ctx, child.SessionID)
	if err != nil || len(plan.Blockers) > 0 {
		t.Fatalf("terminal child held: %+v %v", plan, err)
	}
	if _, err := database.CloseRetention(ctx, child.SessionID, plan.ID, true); err != nil {
		t.Fatal(err)
	}
	goal := createAbandonmentGoal(t, database)
	if _, err := database.Orchestration().StartRound(ctx, goal.ID, 1, "goal-turn", "input"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Orchestration().FinishGoal(ctx, goal.ID, agentgoal.Finish{State: agentgoal.StateCanceled, StopReason: agentgoal.StopCanceled, Reason: "stopped"}); err != nil {
		t.Fatal(err)
	}
	input, _ := agentflow.NewValue("task")
	flowID, _ := agentflow.NewID()
	manifest := runtimeManifest(t, "v1")
	snapshotID, _ := manifest.ID()
	ref := tool.SnapshotReference{Kind: "flow", Scope: string(root.TeamID), ID: "flow-key"}
	if err := database.RuntimeSnapshots().Save(ctx, manifest, ref); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Orchestration().CreateRun(ctx, agentflow.RunRequest{ID: flowID, IdempotencyKey: ref.ID, Definition: "test", DefinitionRevision: "v1", DefinitionFingerprint: strings.Repeat("a", 64), SessionID: root.SessionID, TeamID: root.TeamID, AgentID: root.AgentID, Input: input, RegistryGeneration: 1, RuntimeSnapshotID: snapshotID}); err != nil {
		t.Fatal(err)
	}
	plan, err = database.RetentionPlan(ctx, "root")
	if err != nil || len(plan.Blockers) == 0 {
		t.Fatalf("running flow not held: %+v %v", plan, err)
	}
	if _, err := database.Orchestration().FinishRun(ctx, flowID, agentflow.StateCompleted, []agentflow.Value{input}, ""); err != nil {
		t.Fatal(err)
	}
	plan, err = database.RetentionPlan(ctx, "root")
	if err != nil || len(plan.Blockers) > 0 || len(plan.Consumers) != 2 {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	if _, err := database.CloseRetention(ctx, "root", plan.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := database.RuntimeSnapshots().Save(ctx, manifest, ref); !errors.Is(err, tool.ErrSnapshotReferenceReleased) {
		t.Fatalf("flow reopened: %v", err)
	}
	if stored, err := database.Orchestration().GetRun(ctx, flowID); err != nil || len(stored.Outputs) != 1 {
		t.Fatalf("flow output lost: %+v %v", stored, err)
	}
}

func TestPluginCollectionRequiresClosedConsumersAndKeepsIdentity(t *testing.T) {
	ctx := context.Background()
	database := abandonmentDatabase(t)
	source := []byte("artifact bytes")
	record := catalogRecord("v1", source)
	if err := database.Plugins().Save(ctx, record, source); err != nil {
		t.Fatal(err)
	}
	manifest := runtimeManifest(t, "v1")
	snapshotID, _ := manifest.ID()
	ref := tool.SnapshotReference{Kind: "turn", Scope: "root", ID: "done"}
	if err := database.RuntimeSnapshots().Save(ctx, manifest, ref); err != nil {
		t.Fatal(err)
	}
	recordAbandonmentTurn(t, database, "root", "done", snapshotID, true)
	plan, err := database.PluginCollectionPlan(ctx, "echo", "v1")
	if err != nil || len(plan.Blockers) == 0 {
		t.Fatalf("live refs not held: %+v %v", plan, err)
	}
	if _, err := database.CollectPlugin(ctx, "echo", "v1", plan.ID, true); !errors.Is(err, retention.ErrBlocked) {
		t.Fatalf("collected pinned revision: %v", err)
	}
	closePlan, err := database.RetentionPlan(ctx, "root")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.CloseRetention(ctx, "root", closePlan.ID, true); err != nil {
		t.Fatal(err)
	}
	plan, err = database.PluginCollectionPlan(ctx, "echo", "v1")
	if err != nil || len(plan.Blockers) > 0 || plan.ReclaimableBytes != len(source) {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	if _, err := database.CollectPlugin(ctx, "echo", "v1", plan.ID, false); err == nil {
		t.Fatal("collected without confirmation")
	}
	receipt, err := database.CollectPlugin(ctx, "echo", "v1", plan.ID, true)
	if err != nil || !receipt.BlobDeleted {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	copy, err := database.CollectPlugin(ctx, "echo", "v1", plan.ID, true)
	if err != nil || !reflect.DeepEqual(receipt, copy) {
		t.Fatalf("replay=%+v %v", copy, err)
	}
	if err := database.Plugins().Save(ctx, record, source); !errors.Is(err, plugin.ErrArtifactExists) {
		t.Fatalf("reused collected revision: %v", err)
	}
	foreign := catalogRecord("v2", source)
	foreign.Manifest.PluginID = "different-owner"
	if err := database.Plugins().Save(ctx, foreign, source); !errors.Is(err, plugin.ErrToolOwned) {
		t.Fatalf("stole deleted owner: %v", err)
	}
	if items, err := database.Plugins().Load(ctx); err != nil || len(items) != 0 {
		t.Fatalf("collected module loaded: %+v %v", items, err)
	}
	if err := database.Plugins().SetActive(ctx, "echo", "", "v1"); err == nil {
		t.Fatal("activated collected module")
	}
}

func TestPluginCollectionConservativeLegacySharedBlobAndFailure(t *testing.T) {
	for _, mode := range []string{"legacy", "shared", "active", "write-failure", "changed"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			database := abandonmentDatabase(t)
			source := []byte("shared source")
			if err := database.Plugins().Save(ctx, catalogRecord("v1", source), source); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "legacy":
				recordAbandonmentTurn(t, database, "old", "turn", "registry-1", true)
			case "shared":
				if err := database.Plugins().Save(ctx, catalogRecord("v2", source), source); err != nil {
					t.Fatal(err)
				}
			case "active":
				if err := database.Plugins().SetActive(ctx, "echo", "", "v1"); err != nil {
					t.Fatal(err)
				}
			}
			plan, err := database.PluginCollectionPlan(ctx, "echo", "v1")
			if err != nil {
				t.Fatal(err)
			}
			if mode == "write-failure" {
				if _, err := database.db.Exec(`CREATE TRIGGER fail_gc BEFORE INSERT ON maintenance_receipts BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "changed" {
				recordAbandonmentTurn(t, database, "new", "turn", "unknown", false)
			}
			receipt, err := database.CollectPlugin(ctx, "echo", "v1", plan.ID, true)
			if mode == "shared" {
				if err != nil || receipt.BlobDeleted || receipt.Plan.ReclaimableBytes != 0 {
					t.Fatalf("shared blob deleted: %+v %v", receipt, err)
				}
				items, err := database.Plugins().Load(ctx)
				if err != nil || len(items) != 1 || string(items[0].Source) != string(source) {
					t.Fatalf("shared revision lost: %+v %v", items, err)
				}
			} else {
				if err == nil {
					t.Fatal("unsafe GC accepted")
				}
				items, err := database.Plugins().Load(ctx)
				if err != nil || len(items) != 1 {
					t.Fatalf("failed GC lost source: %+v %v", items, err)
				}
			}
		})
	}
}
