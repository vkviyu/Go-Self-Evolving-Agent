package observation

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

const queueDepth = 1024
const segmentBytes = 8 << 20

// Recorder has a single writer. Each invocation has its own private file, so
// processes and sessions can overlap without filesystem coordination.
type Recorder struct {
	path      string
	queue     chan Event
	mu        sync.RWMutex // protects sending vs. closing the channel, never file I/O
	closeOnce sync.Once
	stopped   atomic.Bool
	dropped   atomic.Uint64
	done      chan struct{}
	err       error // published by closing done
}

// Open schedules filesystem setup on the worker. A setup or write error is
// reported by Close; it never propagates into a model, tool or storage call.
func Open(directory string) (*Recorder, error) {
	if directory == "" {
		return nil, errors.New("missing diagnostics directory")
	}
	name := "trace-" + time.Now().UTC().Format("20060102T150405Z") + "-" + rand.Text() + ".jsonl"
	path := filepath.Join(directory, name)
	return start(path, queueDepth, func() (io.WriteCloser, error) {
		out, err := newSegmentWriter(path, segmentBytes)
		if err != nil {
			return nil, err
		}
		expireFinished(directory, time.Now().Add(-7*24*time.Hour))
		return out, nil
	}), nil
}

func start(path string, capacity int, open func() (io.WriteCloser, error)) *Recorder {
	r := &Recorder{path: path, queue: make(chan Event, capacity), done: make(chan struct{})}
	go r.write(open)
	return r
}

func (r *Recorder) Path() string { return r.path }

func (r *Recorder) Attach(ctx context.Context) context.Context {
	return context.WithValue(ctx, recorderKey{}, r)
}

func (r *Recorder) offer(e Event) {
	if !r.mu.TryRLock() {
		return
	}
	defer r.mu.RUnlock()
	if r.stopped.Load() {
		return
	}
	select {
	case r.queue <- e:
	default:
		r.dropped.Add(1)
	}
}

func (r *Recorder) write(open func() (io.WriteCloser, error)) {
	defer close(r.done)
	defer r.stopped.Store(true)
	out, err := open()
	if err != nil {
		r.err = err
		return
	}
	defer func() { r.err = errors.Join(r.err, out.Close()) }()
	encoder := json.NewEncoder(out)
	var seq uint64
	write := func(e Event) error {
		seq++
		e.Version, e.Sequence, e.Dropped = 1, seq, r.dropped.Load()
		return encoder.Encode(e)
	}
	for e := range r.queue {
		if err := write(e); err != nil {
			r.err = err
			return
		}
	}
	r.err = write(Event{At: time.Now().UTC(), Component: "observer", Name: "recorder.closed"})
}

func (r *Recorder) Close(ctx context.Context) error {
	r.closeOnce.Do(func() {
		r.mu.Lock()
		r.stopped.Store(true)
		close(r.queue)
		r.mu.Unlock()
	})
	select {
	case <-r.done:
		return r.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// The current segment and its immediate predecessor bound each run's disk use.
// A line is encoded in a single Write by json.Encoder.
type segmentWriter struct {
	file        *os.File
	path        string
	size, limit int64
}

func newSegmentWriter(path string, limit int64) (*segmentWriter, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	w := &segmentWriter{path: path, limit: limit}
	return w, w.open()
}

func (w *segmentWriter) open() error {
	f, err := os.OpenFile(w.path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	w.file, w.size = f, 0
	return nil
}

func (w *segmentWriter) Write(p []byte) (int, error) {
	if w.size > 0 && w.size+int64(len(p)) > w.limit {
		if err := w.Close(); err != nil {
			return 0, err
		}
		if err := os.Rename(w.path, w.path+".1"); err != nil {
			return 0, err
		}
		if err := w.open(); err != nil {
			return 0, err
		}
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *segmentWriter) Close() error {
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}
