package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	orchestrationate "github.com/vkviyu/go-self-evolving-agent/internal/orchestration/atestore"
	"github.com/vkviyu/go-self-evolving-agent/internal/runtimeowner"
	"github.com/vkviyu/go-self-evolving-agent/internal/runtimestore"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

func TestAbandonRecoveryRequiresExclusiveOfflineOwner(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t, config.Config{})
	application, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.Close() })
	if _, _, err := application.EnsureRootThread(ctx, "root"); err != nil {
		t.Fatal(err)
	}
	manifest, err := tool.NewRegistry().Snapshot().Manifest()
	if err != nil {
		t.Fatal(err)
	}
	snapshotID, _ := manifest.ID()
	ref := tool.SnapshotReference{Kind: "turn", Scope: "root", ID: "target"}
	if err := application.runtime.snapshots.Save(ctx, manifest, ref); err != nil {
		t.Fatal(err)
	}
	checkpointID := "session/root/turn/target"
	if err := application.runtime.checkpoints.Set(ctx, checkpointID, []byte("opaque evidence")); err != nil {
		t.Fatal(err)
	}
	if _, err := application.events.Append(ctx, "root", session.AnySequence, session.PendingEvent{Type: session.EventTurnStarted, CorrelationID: "target", Payload: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	request := session.AbandonRequest{SessionID: "root", TurnID: "target", Reason: "effects reviewed", EffectsReviewed: true}
	if _, err := AbandonRecovery(ctx, cfg, request); !errors.Is(err, runtimeowner.ErrOwned) {
		t.Fatalf("live runtime was not excluded: %v", err)
	}
	// A fresh local runtime database must not bypass the shared Session lock.
	other := testConfig(t, config.Config{ATE: cfg.ATE})
	other.ATE.Timeout = 100 * time.Millisecond
	if _, err := AbandonRecovery(ctx, other, request); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("different runtime path bypassed Session ownership: %v", err)
	}
	if err := session.CheckRecovery(ctx, application.events, "root", "target"); err != nil {
		t.Fatal("failed ownership still changed recovery", err)
	}
	if err := application.Close(); err != nil {
		t.Fatal(err)
	}
	missing := request
	missing.TurnID = "missing"
	if _, err := AbandonRecovery(ctx, cfg, missing); err == nil {
		t.Fatal("missing target accepted")
	}
	if receipt, err := AbandonRecovery(ctx, cfg, request); err != nil || receipt.ReleasedReferences != 1 {
		t.Fatalf("offline failure leaked ownership or cleanup failed: %+v %v", receipt, err)
	}
	application, err = Open(ctx, cfg)
	if err != nil {
		t.Fatal("successful abandonment leaked ownership", err)
	}
	if _, _, err := application.EnsureRootThread(ctx, "root"); err != nil {
		t.Fatal("successful abandonment leaked shared Session ownership", err)
	}
	if err := session.CheckRecovery(ctx, application.events, "root", "target"); !errors.Is(err, session.ErrRecoveryAbandoned) {
		t.Fatalf("abandonment missing: %v", err)
	}
	if _, found, err := application.runtime.checkpoints.Get(ctx, checkpointID); err != nil || found {
		t.Fatalf("checkpoint not cleaned: found=%v err=%v", found, err)
	}
	if refs, err := application.runtime.snapshots.References(ctx, snapshotID); err != nil || len(refs) != 0 {
		t.Fatalf("references not released: %+v %v", refs, err)
	}
	if err := application.runtime.snapshots.Save(ctx, manifest, ref); !errors.Is(err, tool.ErrSnapshotReferenceReleased) {
		t.Fatalf("abandoned native turn reopened: %v", err)
	}
}

type ownershipCheckingAuthority struct {
	abandonmentAuthority
	check func(string)
}

func (s ownershipCheckingAuthority) AbandonRecovery(ctx context.Context, request session.AbandonRequest) (session.Abandonment, error) {
	s.check("before authoritative mutation")
	receipt, err := s.abandonmentAuthority.AbandonRecovery(ctx, request)
	s.check("after authoritative mutation")
	return receipt, err
}

type ownershipCheckingCleanup struct {
	abandonedReferences
	session.CheckpointStore
	check func(string)
}

func (s ownershipCheckingCleanup) CloseAbandonedReferences(ctx context.Context, receipt session.Abandonment) (int, error) {
	s.check("before reference cleanup")
	n, err := s.abandonedReferences.CloseAbandonedReferences(ctx, receipt)
	s.check("after reference cleanup")
	return n, err
}

func (s ownershipCheckingCleanup) Delete(ctx context.Context, id string) error {
	s.check("before checkpoint cleanup")
	err := s.CheckpointStore.Delete(ctx, id)
	s.check("after checkpoint cleanup")
	return err
}

func TestOfflineAbandonHoldsSharedSessionOwnerThroughCleanup(t *testing.T) {
	ctx := t.Context()
	cfg := testConfig(t, config.Config{})
	application, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.Close() })
	if _, err := application.events.Append(ctx, "root", 0, session.PendingEvent{
		Type: session.EventTurnStarted, CorrelationID: "turn", Payload: []byte(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	contender := &App{
		config:    testConfig(t, config.Config{ATE: cfg.ATE}),
		authority: &authorityResources{storeID: application.authority.storeID},
	}
	t.Cleanup(func() {
		for _, owner := range contender.sessionOwners {
			_ = owner.Close()
		}
	})
	if err := application.Close(); err != nil {
		t.Fatal(err)
	}
	checks := 0
	check := func(stage string) {
		t.Helper()
		if err := contender.ownSession("root"); !errors.Is(err, runtimeowner.ErrOwned) {
			t.Fatalf("%s lost the shared Session lock: %v", stage, err)
		}
		// The owner excludes exactly one Session, not every Session in ATE.
		if err := contender.ownSession("independent"); err != nil {
			t.Fatalf("%s blocked an independent Session: %v", stage, err)
		}
		checks++
	}
	err = withOfflineSessionAuthority(ctx, cfg, "root", func(repositories *orchestrationate.Store, database *runtimestore.Database) error {
		authority := ownershipCheckingAuthority{abandonmentAuthority: repositories, check: check}
		cleanup := ownershipCheckingCleanup{
			abandonedReferences: database.RuntimeSnapshots(), CheckpointStore: database.Checkpoints(), check: check,
		}
		_, err := abandonRecoveryWithStores(ctx, session.AbandonRequest{
			SessionID: "root", TurnID: "turn", Reason: "reviewed", EffectsReviewed: true,
		}, authority, cleanup, cleanup)
		return err
	})
	if err != nil || checks != 6 {
		t.Fatalf("offline mutation/cleanup: checks=%d err=%v", checks, err)
	}
	if err := contender.ownSession("root"); err != nil {
		t.Fatalf("offline cleanup leaked shared Session lock: %v", err)
	}
}

type interruptedAbandonmentEvents struct {
	session.EventStore
	failure error
}

func (s interruptedAbandonmentEvents) Append(context.Context, session.ID, uint64, ...session.PendingEvent) ([]session.Event, error) {
	return nil, s.failure
}

func TestRootStartupResumesOnlyOwnedSessionAbandonment(t *testing.T) {
	ctx := t.Context()
	cfg := testConfig(t, config.Config{})
	application, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.Close() })
	failure := errors.New("interrupted after preparation")
	repositories, err := orchestrationate.New(application.authority.records, interruptedAbandonmentEvents{
		EventStore: application.events, failure: failure,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []session.ID{"owned", "foreign"} {
		if _, _, err := application.EnsureRootThread(ctx, id); err != nil {
			t.Fatal(err)
		}
		if _, err := application.events.Append(ctx, id, session.AnySequence, session.PendingEvent{
			Type: session.EventTurnStarted, CorrelationID: "turn", Payload: []byte(`{}`),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := repositories.AbandonRecovery(ctx, session.AbandonRequest{
			SessionID: id, TurnID: "turn", Reason: "reviewed", EffectsReviewed: true,
		}); !errors.Is(err, failure) {
			t.Fatalf("failed to leave a prepared receipt for %s: %v", id, err)
		}
	}
	if err := application.Close(); err != nil {
		t.Fatal(err)
	}
	application, err = Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []session.ID{"owned", "foreign"} {
		if err := session.CheckRecovery(ctx, application.events, id, "turn"); err != nil {
			t.Fatalf("Open advanced an unowned Session %s: %v", id, err)
		}
	}
	if _, _, err := application.EnsureRootThread(ctx, "owned"); err != nil {
		t.Fatal(err)
	}
	if err := session.CheckRecovery(ctx, application.events, "owned", "turn"); !errors.Is(err, session.ErrRecoveryAbandoned) {
		t.Fatalf("root admitted before abandonment reconciliation: %v", err)
	}
	if err := session.CheckRecovery(ctx, application.events, "foreign", "turn"); err != nil {
		t.Fatalf("root startup advanced foreign abandonment: %v", err)
	}
	if _, _, err := application.EnsureRootThread(ctx, "foreign"); err != nil {
		t.Fatal(err)
	}
	if err := session.CheckRecovery(ctx, application.events, "foreign", "turn"); !errors.Is(err, session.ErrRecoveryAbandoned) {
		t.Fatalf("foreign receipt lost before its owner started: %v", err)
	}
}
