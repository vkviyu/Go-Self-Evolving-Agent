package app

import (
	"context"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/go-self-evolving-agent/internal/ateprocess"
	"github.com/vkviyu/go-self-evolving-agent/internal/config"
)

type (
	storageTiming struct {
		mu      sync.Mutex
		starts  int
		last    map[uint64]ateprocess.Progress
		elapsed map[ateprocess.Stage]time.Duration
	}
)

// observe records only phase timings, never paths, request bodies or credentials.
func (s *storageTiming) observe(_ context.Context, event ateprocess.Progress) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last == nil {
		s.last = make(map[uint64]ateprocess.Progress)
		s.elapsed = make(map[ateprocess.Stage]time.Duration)
	}
	if event.Stage == ateprocess.StageStart {
		s.starts++
	}
	if previous, ok := s.last[event.BatchID]; ok {
		s.elapsed[previous.Stage] += event.At.Sub(previous.At)
	}
	if event.Stage == ateprocess.StageDone {
		delete(s.last, event.BatchID)
	} else {
		s.last[event.BatchID] = event
	}
}

// TestExistingStorageReadTiming is an explicit, read-only local diagnostic.
// It uses native process ownership and never constructs a model or tool runner.
func TestExistingStorageReadTiming(t *testing.T) {
	if os.Getenv("GSEA_TEST_EXISTING_STORAGE") != "1" {
		t.Skip("opt-in existing-store timing")
	}
	id := os.Getenv("GSEA_TEST_STORAGE_SESSION")
	if id == "" {
		t.Fatal("GSEA_TEST_STORAGE_SESSION is required")
	}
	cfg, err := config.Load(config.LoadOptions{})
	if err != nil {
		t.Fatal("load storage config:", err)
	}
	cfg.ATE.Binary = os.Getenv("GSEA_ATE_BINARY")
	if cfg.ATE.Binary == "" {
		t.Fatal("GSEA_ATE_BINARY must name the existing native binary")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	var opening storageTiming
	start := time.Now()
	a, err := OpenControl(ateprocess.WithObserver(ctx, opening.observe), cfg)
	t.Logf("open: elapsed=%s launches=%d stages=%v", time.Since(start), opening.starts, opening.elapsed)
	if err != nil {
		t.Fatal("open existing authority:", err)
	}
	defer a.Close()
	if os.Getenv("GSEA_TEST_STORAGE_TIMELINE") == "1" {
		err := a.authority.bound.WithClient(ctx, func(c *client.Client) error {
			page, err := c.ListSessionEvents(ctx, api.ListSessionEventsRequest{
				Space: authoritySpace, Principal: "gsea", SessionID: id, Limit: 128,
			})
			if err != nil {
				return err
			}
			t.Logf("timeline: found=%v events=%d through=%d truncated=%v", page.Found, page.Total, page.ThroughSequence, page.Continuation != "")
			for _, item := range page.Items {
				var envelope struct {
					Type       string
					OccurredAt time.Time
					Payload    struct {
						Error string `json:"error"`
					}
				}
				if err := json.Unmarshal(item.Event.Event.Payload, &envelope); err != nil {
					return err
				}
				t.Logf("event: seq=%d type=%q at=%s", item.Event.Sequence, envelope.Type, envelope.OccurredAt.Format(time.RFC3339Nano))
				if envelope.Type == "model.failed" {
					// Provider errors can contain private request text or URLs.
					// Log only recognized diagnostic markers.
					text := strings.ToLower(envelope.Payload.Error)
					t.Logf("model failure: bytes=%d", len(text))
					if match := regexp.MustCompile(`^read responses stream: model stream read timed out after ([0-9hms.]+)$`).FindStringSubmatch(text); match != nil {
						t.Logf("model failure: host stream idle timeout=%s", match[1])
					}
					for _, marker := range []string{"context canceled", "context deadline exceeded", "timeout", "timed out", "connection reset", "eof", "responses stream error:", "send streaming responses request:", "read responses stream:", "incomplete", "max_output_tokens", "upstream", "invalid", "no assistant output"} {
						if strings.Contains(text, marker) {
							t.Logf("model failure marker: %q", marker)
						}
					}
					for _, status := range regexp.MustCompile(`\b[45][0-9]{2}\b`).FindAllString(text, -1) {
						t.Logf("model failure numeric code: %s", status)
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal("read timeline metadata:", err)
		}
		if os.Getenv("GSEA_TEST_STORAGE_TIMELINE_ONLY") == "1" {
			return
		}
	}
	read := func(ctx context.Context) error {
		return a.authority.bound.WithClient(ctx, func(c *client.Client) error {
			_, err := c.GetSession(ctx, api.ReadSessionRequest{
				Space: authoritySpace, Principal: "gsea", SessionID: id,
			})
			return err
		})
	}
	work := func(ctx context.Context) error {
		for range 4 {
			if err := read(ctx); err != nil {
				return err
			}
		}
		return nil
	}
	for _, batched := range []bool{false, true} {
		var timing storageTiming
		traced := ateprocess.WithObserver(ctx, timing.observe)
		start := time.Now()
		if batched {
			err = a.WithStorageBatch(traced, work)
		} else {
			err = work(traced)
		}
		t.Logf("four session reads: batched=%v elapsed=%s launches=%d stages=%v", batched, time.Since(start), timing.starts, timing.elapsed)
		if err != nil {
			t.Fatalf("batched=%v: %v", batched, err)
		}
	}
}
