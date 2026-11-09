package agentstream

import "golang.org/x/sys/unix"

// Claim the pending marker without replacing a marker already being processed.
func movePatchFactsMarkerNoReplace(source, destination string) error {
	return unix.Renameat2(unix.AT_FDCWD, source, unix.AT_FDCWD, destination, unix.RENAME_NOREPLACE)
}
