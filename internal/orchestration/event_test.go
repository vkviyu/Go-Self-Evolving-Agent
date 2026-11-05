package orchestration

import (
	"errors"
	"testing"
)

func TestPendingEventValidation(t *testing.T) {
	t.Parallel()
	teamID, err := NewTeamID()
	if err != nil {
		t.Fatal(err)
	}
	stream := Stream{Type: StreamTeam, ID: string(teamID), TeamID: teamID}
	if err := stream.Validate(); err != nil {
		t.Fatal(err)
	}
	event := PendingEvent{Type: EventTeamCreated, Payload: MustJSONPayload(map[string]string{"source": "test"})}
	if err := event.Validate(); err != nil {
		t.Fatal(err)
	}
	event.Payload = []byte("invalid")
	if err := event.Validate(); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("got %v, want ErrInvalidEvent", err)
	}
}
