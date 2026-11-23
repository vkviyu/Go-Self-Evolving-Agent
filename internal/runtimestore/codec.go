package runtimestore

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// randomID isolates each in-memory runtime database.
func randomID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate random ID: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

// sha256Hex is the hex-encoded SHA-256 digest used for every stored checksum
// and request hash.
func sha256Hex(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}
