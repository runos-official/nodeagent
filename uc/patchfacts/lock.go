package patchfacts

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// packageLockHeld reports whether another package manager holds any of the apt
// or dpkg locks, and names the first lock it finds held.
//
// IT ASKS fcntl, NOT flock. apt takes these locks with an fcntl record lock
// (apt-pkg's GetLock) and so does dpkg, and on Linux an fcntl lock and a flock
// lock do not see each other. A flock probe would therefore report every node
// as free, and the collector would refresh the metadata under a running
// package manager.
//
// IT ASKS WITH F_GETLK, so the probe acquires nothing. F_GETLK is a question:
// it reports the conflicting lock, if any, and needs only a readable file
// descriptor. A probe that took the lock to find out whether it was free would
// itself be a write, and would make the collector the thing it is checking for.
//
// A lock file that does not exist is not a held lock: apt creates these files
// when it first needs them.
func packageLockHeld() (bool, string, error) {
	for _, path := range lockPaths {
		held, err := fcntlLockHeld(path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return false, "", fmt.Errorf("cannot probe %s: %w", path, err)
		}
		if held {
			return true, path, nil
		}
	}
	return false, "", nil
}

// fcntlLockHeld asks fcntl whether a write lock on the whole of path would
// conflict with a lock another process already holds.
func fcntlLockHeld(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()

	lk := syscall.Flock_t{
		Type:   syscall.F_WRLCK,
		Whence: 0,
		Start:  0,
		Len:    0,
	}
	if err := syscall.FcntlFlock(f.Fd(), syscall.F_GETLK, &lk); err != nil {
		return false, err
	}
	// F_GETLK leaves Type as F_UNLCK when nothing conflicts, and otherwise
	// fills it with the conflicting lock's type.
	return lk.Type != syscall.F_UNLCK, nil
}
