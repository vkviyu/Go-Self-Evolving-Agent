package app

import (
	"context"
	"errors"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/runtimeowner"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/sqlitestore"
)

func TestAbandonRecoveryRequiresExclusiveOfflineOwner(t *testing.T) {
	ctx := context.Background()
	cfg := config.Config{DataDirectory: t.TempDir(), WorkspaceDirectory: t.TempDir()}
	application, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.Close() })
	if _, err := application.events.Append(ctx, "root", session.AnySequence, session.PendingEvent{Type: session.EventTurnStarted, CorrelationID: "target", Payload: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	request := session.AbandonRequest{SessionID: "root", TurnID: "target", Reason: "effects reviewed", EffectsReviewed: true}
	if _, err := AbandonRecovery(ctx, cfg, request); !errors.Is(err, runtimeowner.ErrOwned) {
		t.Fatalf("live runtime was not excluded: %v", err)
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
	if _, err := AbandonRecovery(ctx, cfg, request); err != nil {
		t.Fatal("offline failure leaked ownership", err)
	}
	owner, err := runtimeowner.Acquire(cfg.SessionDatabasePath())
	if err != nil {
		t.Fatal("successful abandonment leaked ownership", err)
	}
	defer owner.Close()
	database, err := sqlitestore.Open(ctx, cfg.SessionDatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := session.CheckRecovery(ctx, database.SessionEvents(), "root", "target"); !errors.Is(err, session.ErrRecoveryAbandoned) {
		t.Fatalf("abandonment missing: %v", err)
	}
}
