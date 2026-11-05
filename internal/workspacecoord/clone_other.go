//go:build !darwin && !linux

package workspacecoord

// cloneFile reports that this platform has no copy-on-write file clone, so
// snapshots always use the bounded plain copy.
func cloneFile(src, dst string) error {
	return errCloneUnsupported
}
