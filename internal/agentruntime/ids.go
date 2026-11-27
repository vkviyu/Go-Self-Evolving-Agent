package agentruntime

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

func checkpointID(sessionID session.ID, turnID string) string {
	return "session/" + string(sessionID) + "/turn/" + turnID
}

// randomID generates a 128-bit random identifier encoded as hexadecimal.
func randomID() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", fmt.Errorf("generate turn ID: %w", err)
	}
	return hex.EncodeToString(bytes[:]), nil
}
