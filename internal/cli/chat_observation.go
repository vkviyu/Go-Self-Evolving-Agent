package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/observation"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

// The session catalog may select a different runtime directory on resume.
// Open one diagnostic trace for the invocation in the selected directory.
func observeChat(ctx context.Context, data string, id session.ID, interactive bool, stderr io.Writer) (context.Context, func(error)) {
	recorder, err := observation.Open(filepath.Join(data, "diagnostics"))
	if err != nil {
		fmt.Fprintf(stderr, "GSEA execution diagnostics unavailable: %v\n", err)
		return ctx, func(error) {}
	}
	ctx = observation.WithScope(recorder.Attach(ctx), observation.Scope{SessionID: string(id)})
	mode := "headless"
	if interactive {
		mode = "tui"
	}
	observation.Emit(ctx, observation.Event{Component: "cli", Name: "chat.started", Labels: map[string]string{"mode": mode}})
	return ctx, func(result error) {
		status := "completed"
		switch {
		case errors.Is(result, context.Canceled):
			status = "canceled"
		case result != nil:
			status = "failed"
		}
		observation.Emit(ctx, observation.Event{Component: "cli", Name: "chat.finished", Status: status})
		flush, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		if err := recorder.Close(flush); err != nil {
			fmt.Fprintf(stderr, "GSEA execution diagnostics incomplete: %v\n", err)
		}
	}
}
