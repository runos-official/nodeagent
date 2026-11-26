package certstore

import (
	"context"
	"crypto/x509"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/runos-official/nodeagent/uc/csrcert/csrtest"
)

func TestLoadKeyPairDuringRename(t *testing.T) {
	ca := csrtest.NewCA(t)

	t.Run("a read between the key and certificate renames succeeds", func(t *testing.T) {
		f := newFixture(t, ca, v2Leaf, 0o600)
		type result struct {
			leaf *x509.Certificate
			err  error
		}
		done := make(chan result, 1)
		var reads atomic.Int32
		old := loadReadHook
		loadReadHook = func() { reads.Add(1) }
		t.Cleanup(func() { loadReadHook = old })

		s := Open(f.paths)
		s.hook = func(at string) error {
			if at != "renamed-key" {
				return nil
			}
			// The live key is the child, the live certificate is still the
			// parent. Start a reader now and let it see the mismatch.
			go func() {
				pair, err := LoadKeyPair(f.paths.Cert, f.paths.Key)
				var leaf *x509.Certificate
				if err == nil {
					leaf, err = x509.ParseCertificate(pair.Certificate[0])
				}
				done <- result{leaf, err}
			}()
			deadline := time.Now().Add(2 * time.Second)
			for reads.Load() == 0 && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			time.Sleep(50 * time.Millisecond)
			return nil
		}
		if err := prepareAndAdopt(s, f.child); err != nil {
			t.Fatal(err)
		}
		r := <-done
		if r.err != nil {
			t.Fatalf("LoadKeyPair during the rename window: %v", r.err)
		}
		if reads.Load() < 2 {
			t.Fatalf("reads = %d; the reader never saw the mismatch", reads.Load())
		}
		childLeaf, _ := parseOneCert(f.child.CertPEM)
		if !r.leaf.Equal(childLeaf) {
			t.Fatal("the reader did not get the child certificate")
		}
	})

	t.Run("a permanent mismatch fails after 5 reads", func(t *testing.T) {
		f := newFixture(t, ca, v2Leaf, 0o600)
		writeLive(t, f.paths, Material{KeyPEM: f.child.KeyPEM, CertPEM: f.parent.CertPEM, CAPEM: f.parent.CAPEM}, 0o600)
		var reads atomic.Int32
		old := loadReadHook
		loadReadHook = func() { reads.Add(1) }
		t.Cleanup(func() { loadReadHook = old })
		start := time.Now()
		_, err := LoadKeyPair(f.paths.Cert, f.paths.Key)
		if err == nil {
			t.Fatal("want a mismatch error")
		}
		if reads.Load() != 5 {
			t.Fatalf("reads = %d, want 5", reads.Load())
		}
		if el := time.Since(start); el < 400*time.Millisecond {
			t.Fatalf("elapsed %v, want at least 4 waits of 100 ms", el)
		}
	})

	t.Run("a missing file fails at once", func(t *testing.T) {
		d := t.TempDir()
		var reads atomic.Int32
		old := loadReadHook
		loadReadHook = func() { reads.Add(1) }
		t.Cleanup(func() { loadReadHook = old })
		if _, err := LoadKeyPair(filepath.Join(d, "c"), filepath.Join(d, "k")); err == nil {
			t.Fatal("want an error")
		}
		if reads.Load() != 1 {
			t.Fatalf("reads = %d, want 1", reads.Load())
		}
	})
}

func TestRenewalLock(t *testing.T) {
	dir := t.TempDir()
	release, err := AcquireRenewalLock(context.Background(), dir, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := AcquireRenewalLock(context.Background(), dir, 600*time.Millisecond); !errors.Is(err, ErrRenewalInProgress) {
		t.Fatalf("want ErrRenewalInProgress, got %v", err)
	} else if err.Error() != "renewal already in progress" {
		t.Fatalf("text %q", err.Error())
	}
	if el := time.Since(start); el < 600*time.Millisecond {
		t.Fatalf("gave up after %v, want the full wait", el)
	}

	// A waiter gets the lock once the holder releases it.
	got := make(chan error, 1)
	go func() {
		r, err := AcquireRenewalLock(context.Background(), dir, 3*time.Second)
		if err == nil {
			r()
		}
		got <- err
	}()
	time.Sleep(300 * time.Millisecond)
	release()
	if err := <-got; err != nil {
		t.Fatalf("after release: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, RenewalLockName)); err != nil {
		t.Fatalf("lock file: %v", err)
	}

	// A context end stops the wait.
	r2, err := AcquireRenewalLock(context.Background(), dir, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer r2()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := AcquireRenewalLock(ctx, dir, 10*time.Second); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want the context error, got %v", err)
	}
}
