package workspacecoord

import "testing"

// DisableCloneForTest forces plain copies for the rest of a test.
func DisableCloneForTest(t *testing.T) {
	previous := cloneEntry
	cloneEntry = func(string, string) error { return errCloneUnsupported }
	t.Cleanup(func() { cloneEntry = previous })
}
