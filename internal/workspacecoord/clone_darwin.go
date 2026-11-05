//go:build darwin

package workspacecoord

import "golang.org/x/sys/unix"

// cloneFile creates dst as an APFS copy-on-write clone of the regular file
// src. CLONE_NOFOLLOW keeps a racing symlink swap from cloning its target.
func cloneFile(src, dst string) error {
	return unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW)
}
