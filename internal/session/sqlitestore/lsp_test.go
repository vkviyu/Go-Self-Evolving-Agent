package sqlitestore

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/lsp"
)

func TestLSPDiagnosticProjectionRoundTrip(t *testing.T) {
	t.Parallel()
	database, err := Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	want := lsp.Projection{
		WorkspaceRoot: "/workspace", Path: "main.go", Revision: "revision-1", UpdatedAt: time.Now().UTC().Truncate(time.Nanosecond),
		Report: lsp.Report{Status: lsp.StatusReady, Diagnostics: []lsp.Diagnostic{{Path: "main.go", Severity: "error", Message: "broken"}}},
	}
	if err := database.LSPDiagnostics().SaveProjection(context.Background(), want); err != nil {
		t.Fatal(err)
	}
	got, found, err := database.LSPDiagnostics().LoadProjection(context.Background(), want.WorkspaceRoot, want.Path, want.Revision)
	if err != nil || !found {
		t.Fatalf("load projection: found=%v err=%v", found, err)
	}
	if got.Report.Status != lsp.StatusReady || len(got.Report.Diagnostics) != 1 || got.Report.Diagnostics[0].Message != "broken" {
		t.Fatalf("projection = %#v", got)
	}
	if !got.UpdatedAt.Equal(want.UpdatedAt) {
		t.Fatalf("updated at = %s, want %s", got.UpdatedAt, want.UpdatedAt)
	}
}
