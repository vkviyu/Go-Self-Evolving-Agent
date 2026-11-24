package conversation

import (
	"errors"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
)

var (
	ErrAmbiguousRecovery    = errors.New("several interrupted foreground turns. Use /recoveries and /resume <turn-id> to choose one")
	ErrNoForegroundRecovery = errors.New("no interrupted foreground turn. Use /recoveries to inspect earlier turns")
)

func foregroundRecoveryTarget(records []agentruntime.TurnRecord) (agentruntime.TurnRecord, error) {
	var focused agentruntime.TurnRecord
	for _, record := range records {
		if record.IsForegroundRequest() && record.ForegroundSequence > focused.ForegroundSequence {
			focused = record
		}
	}
	if focused.TurnID != "" {
		if focused.Completed || focused.Archived || focused.Abandoned {
			return agentruntime.TurnRecord{}, ErrNoForegroundRecovery
		}
		return focused, nil
	}

	// Older sessions have no foreground marker. Every start is a distinct
	// execution. Select implicitly only when one execution is outstanding
	// after the last completed foreground turn.
	ambiguous := false
	for _, record := range records {
		if !record.IsForegroundRequest() {
			continue
		}
		if record.Completed {
			focused = agentruntime.TurnRecord{}
			ambiguous = false
		} else if !record.Archived && !record.Abandoned {
			ambiguous = ambiguous || focused.TurnID != ""
			focused = record
		}
	}
	if ambiguous {
		return agentruntime.TurnRecord{}, ErrAmbiguousRecovery
	}
	if focused.TurnID == "" {
		return agentruntime.TurnRecord{}, ErrNoForegroundRecovery
	}
	return focused, nil
}
