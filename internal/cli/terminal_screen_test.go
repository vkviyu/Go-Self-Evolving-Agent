package cli

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/x/vt"
)

// terminalScreen applies incremental terminal updates before checking what a user sees.
type terminalScreen struct {
	mu      sync.Mutex
	term    *vt.Emulator
	changed chan struct{}
}

func newTerminalScreen(t *testing.T, width, height int, replies io.Writer) *terminalScreen {
	t.Helper()
	screen := &terminalScreen{term: vt.NewEmulator(width, height), changed: make(chan struct{}, 1)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(replies, screen.term)
	}()
	t.Cleanup(func() {
		// Wake the reader before closing the emulator itself.
		_ = screen.term.InputPipe().(io.Closer).Close()
		<-done
		_ = screen.term.Close()
	})
	return screen
}

func (screen *terminalScreen) Write(p []byte) (int, error) {
	screen.mu.Lock()
	defer screen.mu.Unlock()
	n, err := screen.term.Write(p)
	select {
	case screen.changed <- struct{}{}:
	default:
	}
	return n, err
}

func (screen *terminalScreen) expect(t *testing.T, ctx context.Context, markers ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	for {
		screen.mu.Lock()
		text := screen.term.String()
		screen.mu.Unlock()
		found := true
		for _, marker := range markers {
			if !strings.Contains(text, marker) {
				found = false
				break
			}
		}
		if found {
			return text
		}
		select {
		case <-screen.changed:
		case <-ctx.Done():
			t.Fatalf("waiting for %q: %v\n%s", markers, ctx.Err(), text)
		}
	}
}
