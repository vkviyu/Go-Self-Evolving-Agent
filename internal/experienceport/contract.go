// Package experienceport defines the shared experience data, callback, and
// limits contract without depending on service or storage implementations.
package experienceport

import (
	"context"

	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/agent-temporal-engine/memory"
	"github.com/vkviyu/agent-temporal-engine/model"
)

const (
	MaxSpaces        = 32
	MaxContextBytes  = 32 << 10
	MaxHistoryEvents = 8192
	MaxHistoryBytes  = 16 << 20
	// Native state stores the goal as a JSON string; keep framing headroom.
	MaxGoalBytes = 1<<20 - 64
)

type (
	Generate func(context.Context, []*schema.Message) (*schema.Message, error)

	// Context is an immutable, serializable selection, not an authorization
	// token. Persist the entire value with pending requests and re-run Check.
	Context struct {
		Text        string
		Sources     []memory.Source
		SelectionID string
		SessionID   string
		TurnID      string
		Goal        string
		Execution   model.KnowledgeKey
		Memories    []Selection
		// Evidence is recovered only from the native source authority using
		// checksum-bound references; contexts never embed source event copies.
		EvidenceReferences []EvidenceReference `json:",omitempty"`
		HistoryComplete    bool
		HistoryCoverage    *HistoryCoverage `json:",omitempty"`
	}
	Selection struct {
		Source      memory.Source
		Observation model.ObjectID
		Sequence    uint64
	}
	EvidenceReference struct {
		SourceID       string
		Sequence       uint64
		Kind           string
		OriginalSHA256 string
	}
	// HistoryCoverage describes a primary-evidence projection of a frozen
	// source cut. Exclusions do not delete or acknowledge original events.
	HistoryCoverage struct {
		Policy             string
		Cut                uint64
		Binding            string
		SourceEvents       int
		PrimaryEvents      int
		PrimaryBytes       int
		ExcludedNonprimary map[string]int
		ProjectedWrappers  int
		Complete           bool
	}
)

// Message deliberately returns user-role content. Memory and historical text
// remain untrusted data; callers must never elevate them to system instructions.
func (c Context) Message() *schema.Message { return schema.UserMessage(c.Text) }
