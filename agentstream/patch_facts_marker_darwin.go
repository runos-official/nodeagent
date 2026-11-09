package agentstream

import "golang.org/x/sys/unix"

// Use the same atomic no-replace operation when the release gates run on macOS.
func movePatchFactsMarkerNoReplace(source, destination string) error {
	return unix.RenamexNp(source, destination, unix.RENAME_EXCL)
}
