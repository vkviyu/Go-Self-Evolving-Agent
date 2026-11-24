package session

import (
	"context"
	"errors"
)

// visitPageSize bounds each EventStore.Load call of a VisitEvents scan.
const visitPageSize = 1000

// ErrStopVisit lets a VisitEvents visitor end the scan early once it has
// found what it needs. The scan then reports success.
var ErrStopVisit = errors.New("stop session event visit")

// VisitEvents streams every event of a session in sequence order. A visitor
// error other than ErrStopVisit aborts the scan and is returned as is.
func VisitEvents(ctx context.Context, events EventStore, id ID, visit func(Event) error) error {
	return VisitEventsAfter(ctx, events, id, 0, visit)
}

// VisitEventsAfter streams events strictly newer than the supplied sequence.
func VisitEventsAfter(ctx context.Context, events EventStore, id ID, after uint64, visit func(Event) error) error {
	for {
		batch, err := events.Load(ctx, id, after, visitPageSize)
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			return nil
		}
		for _, event := range batch {
			if err := visit(event); err != nil {
				if errors.Is(err, ErrStopVisit) {
					return nil
				}
				return err
			}
			after = event.Sequence
		}
	}
}
