package certstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// RenewalLockName is the file of the renewal lock, beside the certificate.
// The file is never deleted. It is separate from the agent lock.
const RenewalLockName = ".mtls.renewal.lock"

// ErrRenewalInProgress: another process holds the renewal lock.
var ErrRenewalInProgress = errors.New("renewal already in progress")

const (
	defaultLockWait = 60 * time.Second
	lockTryInterval = 250 * time.Millisecond
	renewalLockMode = 0o644
)

// AcquireRenewalLock takes an exclusive flock on RenewalLockName in dir. It
// tries every 250 ms until wait ends (zero means 60 s), then returns
// ErrRenewalInProgress. When ctx ends first it returns the context error.
// The returned function releases the lock.
func AcquireRenewalLock(ctx context.Context, dir string, wait time.Duration) (release func(), err error) {
	if wait <= 0 {
		wait = defaultLockWait
	}
	path := filepath.Join(dir, RenewalLockName)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, renewalLockMode)
	if err != nil {
		return nil, fmt.Errorf("open renewal lock %s: %w", path, err)
	}
	deadline := time.Now().Add(wait)
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return func() {
				_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
			f.Close()
			return nil, fmt.Errorf("lock %s: %w", path, err)
		}
		left := time.Until(deadline)
		if left <= 0 {
			f.Close()
			return nil, ErrRenewalInProgress
		}
		t := time.NewTimer(min(lockTryInterval, left))
		select {
		case <-ctx.Done():
			t.Stop()
			f.Close()
			return nil, ctx.Err()
		case <-t.C:
		}
	}
}
