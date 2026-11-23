package app

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/model"
	"github.com/vkviyu/go-self-evolving-agent/internal/aterequest"
	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/runtimeowner"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

type offlineAuthorityStub struct {
	receipt session.Abandonment
	err     error
	durable bool
}

func (s *offlineAuthorityStub) AbandonRecovery(context.Context, session.AbandonRequest) (session.Abandonment, error) {
	if s.err != nil {
		return session.Abandonment{}, s.err
	}
	s.durable = true
	return s.receipt, nil
}

type offlineCleanupStub struct {
	authority      *offlineAuthorityStub
	failReferences bool
	failDelete     string
	cleaned        bool
	deleted        map[string]bool
}

func (s *offlineCleanupStub) CloseAbandonedReferences(context.Context, session.Abandonment) (int, error) {
	if !s.authority.durable {
		panic("cleanup preceded authority")
	}
	if s.failReferences {
		return 0, errors.New("reference cleanup failed")
	}
	s.cleaned = true
	return 2, nil
}
func (s *offlineCleanupStub) Get(context.Context, string) ([]byte, bool, error) {
	return nil, false, nil
}
func (s *offlineCleanupStub) Set(context.Context, string, []byte) error {
	return errors.New("unexpected checkpoint write")
}
func (s *offlineCleanupStub) Delete(_ context.Context, id string) error {
	if !s.authority.durable || !s.cleaned {
		panic("checkpoint cleanup preceded durable proof")
	}
	if id == s.failDelete {
		return errors.New("checkpoint cleanup failed")
	}
	s.deleted[id] = true
	return nil
}

func TestOfflineAbandonAuthorityAndCleanupFailures(t *testing.T) {
	for _, stage := range []string{"authority", "references", "second checkpoint"} {
		t.Run(stage, func(t *testing.T) {
			request := session.AbandonRequest{SessionID: "root", TurnID: "one", Reason: "unknown effects reviewed, not undone", EffectsReviewed: true}
			authority := &offlineAuthorityStub{receipt: session.Abandonment{
				SessionID: "root", TurnIDs: []string{"one", "two"}, Reason: request.Reason, AbandonedAt: time.Now().UTC(),
			}}
			cleanup := &offlineCleanupStub{authority: authority, deleted: make(map[string]bool)}
			switch stage {
			case "authority":
				authority.err = errors.New("authority commit unknown")
			case "references":
				cleanup.failReferences = true
			case "second checkpoint":
				cleanup.failDelete = "session/root/turn/two"
			}
			receipt, err := abandonRecoveryWithStores(t.Context(), request, authority, cleanup, cleanup)
			if err == nil {
				t.Fatal("injected failure was ignored")
			}
			if stage == "authority" && (cleanup.cleaned || len(cleanup.deleted) != 0 || receipt.SessionID != "") {
				t.Fatal("authority failure authorized local cleanup")
			}
			if stage != "authority" && receipt.SessionID != "root" {
				t.Fatal("cleanup failure lost durable receipt")
			}
			if stage == "second checkpoint" && !cleanup.deleted["session/root/turn/one"] {
				t.Fatal("did not exercise partial checkpoint cleanup")
			}
			authority.err, cleanup.failReferences, cleanup.failDelete = nil, false, ""
			first, err := abandonRecoveryWithStores(t.Context(), request, authority, cleanup, cleanup)
			if err != nil {
				t.Fatal(err)
			}
			second, err := abandonRecoveryWithStores(t.Context(), request, authority, cleanup, cleanup)
			if err != nil || !reflect.DeepEqual(first, second) || first.EffectsReverted || first.ReleasedReferences != 2 || len(cleanup.deleted) != 2 {
				t.Fatalf("retry changed receipt/evidence: %+v %+v %v", first, second, err)
			}
		})
	}
}

func TestOfflineRetentionFailsClosedWithoutOpeningStorage(t *testing.T) {
	// Invalid input or missing authority configuration must fail before storage
	// opens, whether the maintenance feature is implemented or unavailable.
	cfg := config.Config{DataDirectory: ""}
	ctx := t.Context()
	if _, err := PreviewRetention(ctx, cfg, ""); err == nil {
		t.Fatal("missing session accepted")
	}
	if _, err := ArchiveSession(ctx, cfg, "root", "plan", false); err == nil {
		t.Fatal("unconfirmed archive accepted")
	}
	_, pluginPreviewErr := PreviewPluginCollection(ctx, cfg, "tool", "v1")
	_, collectErr := CollectPlugin(ctx, cfg, "tool", "v1", "plan", true)
	for _, err := range []error{pluginPreviewErr, collectErr} {
		if err == nil {
			t.Fatal("maintenance accepted missing authority configuration")
		}
	}
}

type offlineBindingStub struct {
	authorityPendingStore
	bound bool
}

func TestOfflineRetentionNativeRoundTrip(t *testing.T) {
	ctx := t.Context()
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
	snapshot, _ := manifest.ID()
	ref := tool.SnapshotReference{Kind: "turn", Scope: "root", ID: "turn"}
	if err := application.runtime.snapshots.Save(ctx, manifest, ref); err != nil {
		t.Fatal(err)
	}
	if _, err := application.events.Append(ctx, "root", session.AnySequence,
		session.PendingEvent{Type: session.EventTurnStarted, CorrelationID: "turn", RuntimeSnapshotID: snapshot, Payload: []byte(`{}`)},
		session.PendingEvent{Type: session.EventTurnCompleted, CorrelationID: "turn", RuntimeSnapshotID: snapshot, Payload: []byte(`{"content":"evidence retained"}`)},
	); err != nil {
		t.Fatal(err)
	}
	if err := application.Close(); err != nil {
		t.Fatal(err)
	}
	plan, err := PreviewRetention(ctx, cfg, "root")
	if err != nil || len(plan.Blockers) != 0 || len(plan.Consumers) != 1 {
		t.Fatalf("native preview=%+v %v", plan, err)
	}
	first, err := ArchiveSession(ctx, cfg, "root", plan.ID, true)
	if err != nil || first.ReleasedReferences != 1 || first.EvidenceDeleted {
		t.Fatalf("native archive=%+v %v", first, err)
	}
	second, err := ArchiveSession(ctx, cfg, "root", plan.ID, true)
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatalf("offline archive retry changed receipt: %+v %+v %v", first, second, err)
	}
	application, err = Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := application.EnsureRootThread(ctx, "root"); err != nil {
		t.Fatal(err)
	}
	if err := application.runtime.snapshots.Save(ctx, manifest, ref); !errors.Is(err, tool.ErrSnapshotReferenceReleased) {
		t.Fatalf("archived turn reopened: %v", err)
	}
	if history, err := application.runtime.snapshots.ReferenceHistory(ctx, snapshot); err != nil || len(history) != 1 || history[0].ReleasedAt == nil {
		t.Fatalf("archive reference evidence=%+v %v", history, err)
	}
	if err := session.CheckRecovery(ctx, application.events, "root", "turn"); !errors.Is(err, session.ErrRecoveryArchived) {
		t.Fatalf("native archive fact missing: %v", err)
	}
}

func (s *offlineBindingStub) BindAuthority(context.Context, model.StoreID) error {
	s.bound = true
	return nil
}

func TestOfflineSessionBindingUsesExecutionLock(t *testing.T) {
	shared := t.TempDir()
	newOwner := func() *App {
		return &App{config: config.Config{DataDirectory: t.TempDir(), ATE: config.ATEConfig{Directory: shared}}}
	}
	active := newOwner()
	active.authority = &authorityResources{storeID: "store"}
	if err := active.ownSession("root"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = active.sessionOwners["root"].Close() })
	holder := newOwner()
	t.Cleanup(func() {
		for _, lock := range holder.sessionOwners {
			_ = lock.Close()
		}
	})
	stub := &offlineBindingStub{}
	pending := &offlineSessionPending{authorityPendingStore: stub, owner: holder, id: "root"}
	if err := pending.BindAuthority(t.Context(), "store"); !errors.Is(err, runtimeowner.ErrOwned) {
		t.Fatalf("offline operation bypassed execution lock: %v", err)
	}
	if stub.bound {
		t.Fatal("pending recovery reached binding while session was busy")
	}
	pending.id = "other"
	if err := pending.BindAuthority(t.Context(), "store"); err != nil || !stub.bound {
		t.Fatalf("independent session rejected: %v", err)
	}
	record := aterequest.Record{}
	pending.preexisting = map[api.RequestKey]bool{record.Request.Key(): true}
	if _, err := pending.BeginAttempt(t.Context(), record); err == nil {
		t.Fatal("unscoped preexisting command admitted")
	}
}
