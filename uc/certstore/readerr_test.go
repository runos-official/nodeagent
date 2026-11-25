package certstore

import (
	"errors"
	"io/fs"
	"strings"
	"syscall"
	"testing"

	"github.com/runos-official/nodeagent/uc/csrcert/csrtest"
)

// failReads makes every read of a path that ends with one of suffixes fail
// with EIO until the returned function runs. The file stays on disk and
// unchanged.
func failReads(t *testing.T, suffixes ...string) (resume func()) {
	t.Helper()
	old := readFile
	on := true
	readFile = func(path string) ([]byte, error) {
		for _, s := range suffixes {
			if on && strings.HasSuffix(path, s) {
				return nil, &fs.PathError{Op: "read", Path: path, Err: syscall.EIO}
			}
		}
		return old(path)
	}
	t.Cleanup(func() { readFile = old })
	return func() { on = false }
}

// Finding 1731: a read error is not a missing file, a wrong digest or a
// failed check. Recover returns the error, changes nothing, and adopts the
// same child once reads work again.
func TestRecoverKeepsStageOnReadError(t *testing.T) {
	ca := csrtest.NewCA(t)
	cases := []struct {
		name    string
		crash   string
		damage  string // staged file to damage before Recover, "" for none
		unread  []string
		want    string // "child" or "parent" after reads resume
		wantErr error
	}{
		{"prepared, staged CA unreadable", "checked", "", []string{"ca.crt.next"}, "child", nil},
		{"prepared, staged key unreadable", "staged-cert", "", []string{"mtls.key.next"}, "child", nil},
		{"prepared, live certificate unreadable", "checked", "", []string{"/mtls.crt"}, "child", nil},
		{"committing, staged CA unreadable", "committed", "", []string{"ca.crt.next"}, "child", nil},
		{"committing after two renames, staged certificate unreadable", "renamed-key", "", []string{"mtls.crt.next"}, "child", nil},
		{"committing, journal unreadable", "committed", "", []string{JournalName}, "child", nil},
		{"prepared, journal unreadable", "checked", "", []string{JournalName}, "child", nil},
		{"committing, damaged stage, previous key unreadable", "renamed-key", "mtls.crt.next", []string{"mtls.key.prev"}, "parent", ErrRecoveredToParent},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, ca, v2Leaf, 0o600)
			adoptCrashState(t, f, c.crash)
			if c.damage != "" {
				damage(t, f.dir+"/"+c.damage)
			}
			snap := snapshot(t, f.dir)
			resume := failReads(t, c.unread...)
			err := Open(f.paths).Recover()
			if !errors.Is(err, syscall.EIO) {
				t.Fatalf("want the read error, got %v", err)
			}
			if errors.Is(err, ErrStoreDamaged) || errors.Is(err, ErrRecoveredToParent) {
				t.Fatalf("a read error must not read as a store verdict: %v", err)
			}
			if snapshot(t, f.dir) != snap {
				t.Fatal("Recover changed the directory after a read error")
			}

			// A legitimate retry after reads resume finishes the adoption.
			resume()
			if err := Open(f.paths).Recover(); !errors.Is(err, c.wantErr) {
				t.Fatalf("Recover after reads resume: %v, want %v", err, c.wantErr)
			}
			want := f.child
			if c.want == "parent" {
				want = f.parent
			}
			assertLive(t, f.paths, want, c.name)
			if s := strayFiles(t, f.dir); len(s) != 0 {
				t.Fatalf("stray files remain: %v", s)
			}
		})
	}
}
