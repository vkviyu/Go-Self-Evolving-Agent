// Package sessionregistry associates ATE session identities with host resources.
// Conversation history belongs to native ATE sessions, not to this catalog.
package sessionregistry

import (
	"errors"
	"os"
	"path/filepath"
	"time"
)

const (
	StateCreated State = "created"
	StateRunning State = "running"
	StateStopped State = "stopped"
	StateFailed  State = "failed"
)

type (
	State string

	// Session describes a catalog entry and its local resource locations.
	Session struct {
		ID            string
		Name          string
		Workspace     string
		Runtime       string
		DataDirectory string
		TempDirectory string
		ToolState     string
		State         State
		PID           int
		Model         string
		Provider      string
		CreatedAt     time.Time
		UpdatedAt     time.Time
	}
)

var ErrNotFound = errors.New("session does not exist")

// validState accepts catalog lifecycle states.
func validState(state State) bool {
	return state == StateCreated || state == StateRunning || state == StateStopped || state == StateFailed
}

// createResourceDirectory tracks only newly created directories for rollback.
// Existing resources retain their contents and permissions.
func createResourceDirectory(path string, created *[]string) error {
	if info, err := os.Stat(path); err == nil {
		if !info.IsDir() {
			return errors.New("session resource is not a directory")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	parent := filepath.Dir(path)
	if parent == path {
		return errors.New("session resource has no existing parent")
	}
	if err := createResourceDirectory(parent, created); err != nil {
		return err
	}
	if err := os.Mkdir(path, 0700); err != nil {
		if errors.Is(err, os.ErrExist) {
			info, statErr := os.Stat(path)
			if statErr == nil && info.IsDir() {
				return nil
			}
		}
		return err
	}
	*created = append(*created, path)
	return nil
}
