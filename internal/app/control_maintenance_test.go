package app

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

type controlMaintenanceFixture struct {
	controlRootFixture
	calls []session.ID
	fail  error
}

func (f *controlMaintenanceFixture) ResumeAbandonments(_ context.Context, id session.ID) error {
	f.calls = append(f.calls, id)
	return f.fail
}

func (f *controlMaintenanceFixture) ResumeRetentions(_ context.Context, id session.ID) error {
	f.calls = append(f.calls, id)
	return nil
}

func TestControlScopeMaintenanceRequiresOwnedSession(t *testing.T) {
	root := orchestration.RootThread{
		TeamID: "team_00000000000070008000000000000001", AgentID: "agent_00000000000070008000000000000001",
		ThreadID: "thread_00000000000070008000000000000001", SessionID: "maintenance-owner",
	}
	repository := &controlMaintenanceFixture{controlRootFixture: controlRootFixture{root: root}}
	a := &App{
		config:    config.Config{ATE: config.ATEConfig{Directory: t.TempDir()}},
		authority: &authorityResources{storeID: "maintenance-store"}, repositories: repository,
	}
	t.Cleanup(func() { _ = a.Close() })
	if err := a.resumeControlMaintenance(t.Context(), root.TeamID); err == nil || len(repository.calls) != 0 {
		t.Fatalf("unowned recovery: calls=%v, err=%v", repository.calls, err)
	}
	if err := a.AttachExistingTeam(t.Context(), root.TeamID); err != nil {
		t.Fatal(err)
	}
	if err := a.resumeControlMaintenance(t.Context(), root.TeamID); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(repository.calls, []session.ID{root.SessionID, root.SessionID}) {
		t.Fatalf("maintenance escaped owned Session: %v", repository.calls)
	}
	repository.calls = nil
	repository.fail = errors.New("prepared abandonment cannot finish")
	if err := a.resumeControlMaintenance(t.Context(), root.TeamID); !errors.Is(err, repository.fail) {
		t.Fatalf("failed closure did not block startup: %v", err)
	}
	if !reflect.DeepEqual(repository.calls, []session.ID{root.SessionID}) {
		t.Fatalf("recovery continued after failed closure: %v", repository.calls)
	}
}
