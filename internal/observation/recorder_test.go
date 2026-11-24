package observation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type blockedWriter struct {
	bytes.Buffer
	started, release chan struct{}
	once             sync.Once
}

func (w *blockedWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	<-w.release
	return w.Buffer.Write(p)
}
func (w *blockedWriter) Close() error { return nil }

func TestSlowSinkDoesNotBlockExecutionOrLoseDropAccounting(t *testing.T) {
	out := &blockedWriter{started: make(chan struct{}), release: make(chan struct{})}
	r := start("", 1, func() (io.WriteCloser, error) { return out, nil })
	var release sync.Once
	unblock := func() { release.Do(func() { close(out.release) }) }
	t.Cleanup(unblock)
	ctx := WithScope(r.Attach(t.Context()), Scope{SessionID: "s", TeamID: "team"})
	Emit(ctx, Event{Component: "test", Name: "started"})
	select {
	case <-out.started:
	case <-time.After(2 * time.Second):
		t.Fatal("writer did not start")
	}
	done := make(chan struct{})
	go func() {
		labels := map[string]string{"kind": "original"}
		ev := Event{Component: "test", Name: "child", Labels: labels}
		child := WithScope(ctx, Scope{AgentID: "worker", RequestID: "req"})
		Emit(child, ev)
		labels["kind"] = "changed"
		for i := 0; i < 100; i++ {
			Emit(ctx, ev)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("producer blocked on stalled sink")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := r.Close(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("bounded close: %v", err)
	}
	unblock()
	if err := r.Close(ctx); err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(&out.Buffer)
	var start, child, closed Event
	for _, e := range []*Event{&start, &child, &closed} {
		if err := dec.Decode(e); err != nil {
			t.Fatal(err)
		}
	}
	if child.Sequence != 2 || child.AgentID != "worker" || child.TeamID != "team" || child.SessionID != "s" || child.RequestID != "req" || child.Labels["kind"] != "original" {
		t.Fatal(child)
	}
	if closed.Dropped != 100 || closed.Name != "recorder.closed" || closed.Sequence != 3 {
		t.Fatal(closed)
	}
}

func TestRecorderOpenFailureIsIsolated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	r, err := Open(filepath.Join(path, "unavailable"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := r.Attach(t.Context())
	Emit(ctx, Event{Component: "runtime", Name: "turn.started"})
	if err = r.Close(ctx); err == nil {
		t.Fatal("missing diagnostic sink failure")
	}
	Emit(ctx, Event{Component: "runtime", Name: "turn.finished"})
}

func TestRecorderRotatesOnRecordBoundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	w, err := newSegmentWriter(path, 12)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"first-line\n", "second\n", "third-line\n"} {
		if _, err := io.WriteString(w, line); err != nil {
			t.Fatal(err)
		}
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]string{path: "third-line\n", path + ".1": "second\n"} {
		b, err := os.ReadFile(p)
		if err != nil || string(b) != want {
			t.Fatalf("%s: %q %v", p, b, err)
		}
	}
}

func TestRecorderConcurrentSessionsAndClose(t *testing.T) {
	r, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := r.Attach(t.Context())
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range 8 {
		wg.Go(func() {
			<-start
			for range 100 {
				Emit(ctx, Event{Component: "test", Name: "concurrent", Metrics: map[string]int64{"worker": int64(i)}})
			}
		})
	}
	close(start)
	if err := r.Close(ctx); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if err := r.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestExpiryOnlyRemovesOldFinishedInvocations(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-8 * 24 * time.Hour)
	finished, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := finished.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(finished.Path(), old, old); err != nil {
		t.Fatal(err)
	}
	backup := finished.Path() + ".1"
	if err := os.WriteFile(backup, []byte("previous"), 0600); err != nil {
		t.Fatal(err)
	}
	incomplete := filepath.Join(dir, "trace-20261001T010101Z-ABCDEFGHIJKLMNOPQRSTUVWXYZ.jsonl")
	if err := os.WriteFile(incomplete, []byte("{\"name\":\"turn.started\"}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(incomplete, old, old); err != nil {
		t.Fatal(err)
	}
	expireFinished(dir, time.Now().Add(-7*24*time.Hour))
	for _, p := range []string{finished.Path(), backup} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatal(p, err)
		}
	}
	if _, err := os.Stat(incomplete); err != nil {
		t.Fatal(err)
	}
}
