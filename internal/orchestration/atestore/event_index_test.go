package atestore

import (
	"errors"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/ateauthority"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

func TestEventIndexesSeekPastUnrelatedBodies(t *testing.T) {
	store := newTestStore(t)
	ctx := t.Context()
	first := coreRoot(t, store, "index-first")
	second := coreRoot(t, store, "index-second")
	coreChild(t, store, first)
	coreChild(t, store, second)
	all, err := store.LoadGlobal(ctx, 0, 100)
	coreMust(t, err)
	if len(all) < 4 {
		t.Fatalf("fixture lacks interleaved events: %d", len(all))
	}
	// Corrupt a body outside the requested suffix. A seek must not decode it.
	coreMust(t, store.authority.Update(ctx, func(tx *ateauthority.Tx) error {
		return tx.Put(recordKey("event", string(all[0].EventID)), map[string]any{"invalid": true})
	}))
	suffix, err := store.LoadGlobal(ctx, orchestration.Cursor(all[0].GlobalSequence), 2)
	coreMust(t, err)
	if len(suffix) != 2 || suffix[0].EventID != all[1].EventID || suffix[1].EventID != all[2].EventID {
		t.Fatalf("global index returned wrong suffix: %+v", suffix)
	}
	// Team cursors retain their global meaning across interleaved Teams.
	for _, root := range []orchestration.RootThread{first, second} {
		for _, after := range []uint64{all[0].GlobalSequence, all[2].GlobalSequence, ^uint64(0)} {
			page, err := store.LoadTeam(ctx, root.TeamID, orchestration.Cursor(after), 1)
			coreMust(t, err)
			var want []orchestration.Event
			for _, event := range all {
				if event.TeamID == root.TeamID && event.GlobalSequence > after {
					want = append(want, event)
				}
			}
			if len(want) == 0 {
				if len(page) != 0 {
					t.Fatalf("unexpected tail after %d: %+v", after, page)
				}
			} else if len(page) != 1 || page[0].EventID != want[0].EventID {
				t.Fatalf("Team cursor %d returned %+v, want %s", after, page, want[0].EventID)
			}
		}
	}
	if _, err := store.LoadGlobal(ctx, 0, 1); !errors.Is(err, orchestration.ErrInvalidEvent) {
		t.Fatalf("selected corrupt body was not rejected: %v", err)
	}
}

func TestEventIndexCorruptionDoesNotHideMissingHistory(t *testing.T) {
	store := newTestStore(t)
	ctx := t.Context()
	coreRoot(t, store, "index-missing")
	coreMust(t, store.authority.Update(ctx, func(tx *ateauthority.Tx) error {
		return tx.Delete(eventIndexKey("global", "", 1))
	}))
	if _, err := store.LoadGlobal(ctx, 0, 10); !errors.Is(err, orchestration.ErrInvalidEvent) {
		t.Fatalf("missing index was treated as an empty page: %v", err)
	}
}
