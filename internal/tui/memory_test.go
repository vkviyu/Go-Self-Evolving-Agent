package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/vkviyu/agent-temporal-engine/memory"
	"github.com/vkviyu/go-self-evolving-agent/internal/experience"
)

type memoryBrowserFixture struct {
	items       []experience.MemoryResult
	searchCalls int
}

func (fixture *memoryBrowserFixture) SearchMemories(context.Context, string, int) ([]experience.MemoryResult, error) {
	fixture.searchCalls++
	return fixture.items, nil
}

func (fixture *memoryBrowserFixture) ReadMemories(context.Context, []string) ([]experience.MemoryResult, error) {
	return fixture.items, nil
}

func TestExpandMemoryReferencesKeepsAgentJudgmentBoundary(t *testing.T) {
	model := &Model{
		memoryBrowser: &memoryBrowserFixture{items: []experience.MemoryResult{{
			ID:     "engineering/abc123",
			Memory: memory.Memory{Claim: []byte("Use a bounded retry for transient writes")},
		}}},
	}
	got, err := model.expandMemoryReferences(context.Background(), "Please consider @memory:engineering/abc123")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"explicitly referenced",
		"untrusted evidence",
		"Decide whether each is applicable",
		"Original user request:",
		"Please consider @memory:engineering/abc123",
		"Use a bounded retry for transient writes",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expanded input missing %q: %s", want, got)
		}
	}
}
