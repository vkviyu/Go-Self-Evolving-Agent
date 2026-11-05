package app

import (
	"context"
	"errors"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/runtimeowner"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

func TestAppOwnsDatabaseBeforeRecovery(t *testing.T) {
	configuration := config.Config{DataDirectory: t.TempDir(), WorkspaceDirectory: t.TempDir()}
	first, err := Open(context.Background(), configuration)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	if second, err := Open(context.Background(), configuration); !errors.Is(err, runtimeowner.ErrOwned) {
		if second != nil {
			_ = second.Close()
		}
		t.Fatalf("second runtime opened same database: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := Open(context.Background(), configuration)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestFailedAppAssemblyReleasesOwnership(t *testing.T) {
	configuration := config.Config{DataDirectory: t.TempDir(), WorkspaceDirectory: t.TempDir()}
	if app, err := OpenWithOptions(context.Background(), configuration, OpenOptions{ToolBindings: []tool.Binding{{}}}); err == nil {
		_ = app.Close()
		t.Fatal("invalid tool binding accepted")
	}
	app, err := Open(context.Background(), configuration)
	if err != nil {
		t.Fatalf("failed initialization retained ownership: %v", err)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
}
