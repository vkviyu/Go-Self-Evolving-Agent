package experience

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/agent-temporal-engine/memory"
	"github.com/vkviyu/agent-temporal-engine/model"
)

// MemoryResult is a user-visible, policy-filtered memory candidate. It is
// intentionally separate from experienceport.Selection: viewing or suggesting
// a memory does not adopt it into a turn execution.
type MemoryResult struct {
	ID     string
	Memory memory.Memory
	Source memory.Source
	Score  uint32
}

type MemoryBrowser interface {
	SearchMemories(context.Context, string, int) ([]MemoryResult, error)
	ReadMemories(context.Context, []string) ([]MemoryResult, error)
}

// SearchMemories searches the current reusable memory catalog without asking a
// model to choose or adopting any result.
func (s *Service) SearchMemories(ctx context.Context, text string, limit int) ([]MemoryResult, error) {
	unlock, err := s.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if limit <= 0 || limit > maxSelected*8 {
		limit = maxSelected * 8
	}
	d, err := s.directory(ctx)
	if err != nil {
		return nil, err
	}
	spaces := make([]model.SpaceID, 0, len(d.Spaces))
	for _, space := range d.Spaces {
		spaces = append(spaces, space.ID)
	}
	if len(spaces) == 0 {
		return nil, nil
	}
	return s.queryVisibleMemories(ctx, spaces, text, nil, limit)
}

// ReadMemories resolves exact space/memory references at one policy-checked
// publication cut. It never adopts the records.
func (s *Service) ReadMemories(ctx context.Context, refs []string) ([]MemoryResult, error) {
	unlock, err := s.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if len(refs) == 0 || len(refs) > maxSelected {
		return nil, errors.New("experience: memory reference count is out of bounds")
	}
	grouped := make(map[model.SpaceID][]model.ObjectID)
	for _, ref := range refs {
		space, id, ok := strings.Cut(ref, "/")
		if !ok || space == "" || id == "" {
			return nil, errors.New("experience: memory reference must be <space>/<memory-id>")
		}
		grouped[model.SpaceID(space)] = append(grouped[model.SpaceID(space)], model.ObjectID(id))
	}
	var out []MemoryResult
	for space, ids := range grouped {
		items, err := s.queryVisibleMemories(ctx, []model.SpaceID{space}, "", ids, len(ids))
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
	}
	if len(out) != len(refs) {
		return nil, errors.New("experience: one or more referenced memories are unavailable")
	}
	return out, nil
}

func (s *Service) queryVisibleMemories(ctx context.Context, spaces []model.SpaceID, text string, ids []model.ObjectID, limit int) ([]MemoryResult, error) {
	var items []memoryItem
	err := s.bridge.WithClient(ctx, func(c *client.Client) error {
		var err error
		items, err = queryAll(ctx, c, api.QueryMemoryRequest{
			Spaces: spaces, Text: text, MemoryIDs: ids, AtTime: time.Now().UTC(),
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	if len(items) > limit {
		items = items[:limit]
	}
	out := make([]MemoryResult, 0, len(items))
	for _, item := range items {
		out = append(out, MemoryResult{
			ID: item.ID, Memory: item.Memory, Source: item.Source, Score: item.Score,
		})
	}
	return out, nil
}
