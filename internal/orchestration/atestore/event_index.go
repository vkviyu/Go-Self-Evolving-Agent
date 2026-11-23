package atestore

import (
	"fmt"
	"strconv"

	"github.com/vkviyu/go-self-evolving-agent/internal/ateauthority"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

type eventIndex struct {
	ID     orchestration.EventID
	Global uint64
}

func eventIndexKey(kind, scope string, sequence uint64) string {
	return recordKey("event-index-"+kind, scope) + "/" + strconv.FormatUint(sequence, 10)
}

// Index entries contain only an identity and ordering metadata. The canonical
// event body remains in its one event record. All indexes commit with that body.
func indexEvent(tx *ateauthority.Tx, event orchestration.Event, teamSequence uint64) error {
	entry := eventIndex{ID: event.EventID, Global: event.GlobalSequence}
	for _, key := range []string{
		eventIndexKey("global", "", event.GlobalSequence),
		eventIndexKey("stream", streamCounterID(event.StreamType, event.StreamID), event.StreamSequence),
		eventIndexKey("team", string(event.TeamID), teamSequence),
	} {
		if err := tx.Create(key, entry); err != nil {
			return err
		}
	}
	return nil
}

func readEventIndex(tx *ateauthority.Tx, kind, scope string, sequence uint64) (eventIndex, error) {
	var index eventIndex
	found, err := tx.Get(eventIndexKey(kind, scope, sequence), &index)
	if err != nil {
		return index, err
	}
	if !found || index.ID.Validate() != nil || index.Global == 0 {
		return index, fmt.Errorf("%w: missing or invalid %s event index at %d", orchestration.ErrInvalidEvent, kind, sequence)
	}
	return index, nil
}

func readIndexedEvent(tx *ateauthority.Tx, kind, scope string, sequence uint64) (orchestration.Event, error) {
	index, err := readEventIndex(tx, kind, scope, sequence)
	if err != nil {
		return orchestration.Event{}, err
	}
	event, err := getRecord[orchestration.Event](tx, "event", string(index.ID))
	if err != nil {
		return event, err
	}
	if event.EventID != index.ID || event.GlobalSequence != index.Global || event.TeamID.Validate() != nil {
		return event, fmt.Errorf("%w: event index identity mismatch", orchestration.ErrInvalidEvent)
	}
	valid := kind == "global" && event.GlobalSequence == sequence ||
		kind == "stream" && streamCounterID(event.StreamType, event.StreamID) == scope && event.StreamSequence == sequence ||
		kind == "team" && string(event.TeamID) == scope
	if !valid {
		return event, fmt.Errorf("%w: event index scope mismatch", orchestration.ErrInvalidEvent)
	}
	checksum, err := orchestrationChecksum(event)
	if err != nil {
		return event, err
	}
	if checksum != event.Checksum {
		return event, fmt.Errorf("%w: event %s checksum mismatch", orchestration.ErrInvalidEvent, event.EventID)
	}
	return event, nil
}
