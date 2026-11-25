package certstore

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/runos-official/nodeagent/uc/csrcert/csrtest"
)

// parentPoints are the points before a complete staged child is durable on
// disk. A crash there gives the parent; every other Prepare or Adopt point
// gives the child.
var parentPoints = map[string]bool{
	"prev-ca": true, "prev-key": true, "prev-cert": true, "prepared": true,
	"staged-ca": true, "staged-key": true,
}

func prepareAndAdopt(s *Store, m Material) error {
	if err := s.Prepare(); err != nil {
		return err
	}
	return s.Adopt(m)
}

func TestCrashPointsList(t *testing.T) {
	want := []string{
		"prev-ca", "prev-key", "prev-cert", "prepared",
		"staged-ca", "staged-key", "staged-cert", "checked", "committed",
		"renamed-ca", "renamed-key", "renamed-cert", "dir-synced", "adopted", "prev-removed",
		"recover-committed", "recover-renamed-ca", "recover-renamed-key", "recover-renamed-cert",
		"recover-dir-synced", "recover-adopted", "recover-prev-removed",
		"recover-restored-ca", "recover-restored-key", "recover-restored-cert",
	}
	if strings.Join(CrashPoints, ",") != strings.Join(want, ",") {
		t.Fatalf("CrashPoints = %v\nwant %v", CrashPoints, want)
	}
}

func TestAdoptCrashAtEveryPoint(t *testing.T) {
	ca := csrtest.NewCA(t)
	for _, point := range pointsWith(false) {
		t.Run(point, func(t *testing.T) {
			f := newFixture(t, ca, v2Leaf, 0o600)
			reached := map[string]bool{}
			err := prepareAndAdopt(crashAt(f.paths, point, reached), f.child)
			if !reached[point] {
				t.Fatalf("point %s was never reached (err %v)", point, err)
			}
			if !errors.Is(err, errCrash) {
				t.Fatalf("want the crash at %s, got %v", point, err)
			}

			before, _, _ := Open(f.paths).Adopted()
			if err := Open(f.paths).Recover(); err != nil {
				t.Fatalf("Recover after crash at %s: %v", point, err)
			}
			want, label := f.child, "child"
			if parentPoints[point] {
				want, label = f.parent, "parent"
			}
			assertLive(t, f.paths, want, point+" wants "+label)
			if s := strayFiles(t, f.dir); len(s) != 0 {
				t.Fatalf("%s: stray files remain: %v", point, s)
			}
			rec, ok, err := Open(f.paths).Adopted()
			if err != nil {
				t.Fatalf("%s: journal: %v", point, err)
			}
			if !ok {
				// Only a crash before the first journal write leaves no journal.
				if !parentPoints[point] {
					t.Fatalf("%s: no journal after a child adoption", point)
				}
				return
			}
			if rec.State != StateAdopted {
				t.Fatalf("%s: journal = %+v, want state adopted", point, rec)
			}
			serial, err := Open(f.paths).HeldSerial()
			if err != nil || rec.Serial != serial {
				t.Fatalf("%s: journal serial %q, held serial %q (%v)", point, rec.Serial, serial, err)
			}
			// The sequence rises once, and only when a child becomes live.
			wantSeq := uint64(0)
			if label == "child" {
				wantSeq = 1
			}
			if rec.Sequence != wantSeq {
				t.Fatalf("%s: sequence %d, want %d (before Recover: %+v)", point, rec.Sequence, wantSeq, before)
			}
		})
	}

	// A staged child that fails the checks gives the parent, also at the
	// points where the child would otherwise win.
	for _, point := range []string{"staged-cert", "checked"} {
		t.Run("rejected child at "+point, func(t *testing.T) {
			f := newFixture(t, ca, v2Leaf, 0o600)
			bad := material(t, ca, csrtest.LeafOptions{DNSNames: []string{"acct-b.example.test"}, Version: 2, Type: 1})
			s := crashAt(f.paths, point, nil)
			if err := s.Prepare(); err != nil {
				t.Fatal(err)
			}
			err := s.Adopt(bad)
			if point == "checked" {
				// Adopt rejects the child before it reaches "checked".
				if !errors.Is(err, ErrChildRejected) {
					t.Fatalf("want ErrChildRejected, got %v", err)
				}
			} else if !errors.Is(err, errCrash) {
				t.Fatalf("want crash, got %v", err)
			}
			if err := Open(f.paths).Recover(); err != nil {
				t.Fatalf("Recover: %v", err)
			}
			assertLive(t, f.paths, f.parent, point+" rejected child")
			if s := strayFiles(t, f.dir); len(s) != 0 {
				t.Fatalf("stray files remain: %v", s)
			}
		})
	}
}

// adoptCrashState leaves the store as a crash at point leaves it.
func adoptCrashState(t *testing.T, f *fixture, point string) {
	t.Helper()
	if err := prepareAndAdopt(crashAt(f.paths, point, nil), f.child); !errors.Is(err, errCrash) {
		t.Fatalf("crash at %s: %v", point, err)
	}
}

func damage(t *testing.T, path string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(b, 'X'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverCrashAtEveryPoint(t *testing.T) {
	ca := csrtest.NewCA(t)
	for _, point := range pointsWith(true) {
		t.Run(point, func(t *testing.T) {
			f := newFixture(t, ca, v2Leaf, 0o600)
			restore := strings.HasPrefix(point, "recover-restored-")
			want, wantErr := f.child, error(nil)
			if restore {
				// Two child files are live, the staged certificate is damaged.
				adoptCrashState(t, f, "renamed-key")
				damage(t, f.paths.Cert+".next")
				want, wantErr = f.parent, ErrRecoveredToParent
			} else {
				// A valid child is staged and checked, the commit record is
				// not yet written.
				adoptCrashState(t, f, "checked")
			}
			reached := map[string]bool{}
			if err := crashAt(f.paths, point, reached).Recover(); !errors.Is(err, errCrash) {
				t.Fatalf("Recover crash at %s: %v", point, err)
			}
			if !reached[point] {
				t.Fatalf("point %s was never reached", point)
			}
			err := Open(f.paths).Recover()
			if !errors.Is(err, wantErr) {
				t.Fatalf("second Recover after %s: %v, want %v", point, err, wantErr)
			}
			assertLive(t, f.paths, want, point)
			if s := strayFiles(t, f.dir); len(s) != 0 {
				t.Fatalf("%s: stray files remain: %v", point, s)
			}
			// A further Recover changes nothing.
			snap := snapshot(t, f.dir)
			if err := Open(f.paths).Recover(); err != nil {
				t.Fatalf("third Recover: %v", err)
			}
			if snapshot(t, f.dir) != snap {
				t.Fatalf("%s: third Recover changed the directory", point)
			}
		})
	}
}

func TestRecoverDamagedStage(t *testing.T) {
	ca := csrtest.NewCA(t)
	t.Run("damaged staged certificate restores the parent", func(t *testing.T) {
		f := newFixture(t, ca, v2Leaf, 0o600)
		adoptCrashState(t, f, "renamed-key")
		damage(t, f.paths.Cert+".next")
		err := Open(f.paths).Recover()
		if !errors.Is(err, ErrRecoveredToParent) {
			t.Fatalf("want ErrRecoveredToParent, got %v", err)
		}
		assertLive(t, f.paths, f.parent, "restored")
		if s := strayFiles(t, f.dir); len(s) != 0 {
			t.Fatalf("stray files remain: %v", s)
		}
		rec, _, _ := Open(f.paths).Adopted()
		serial, _ := Open(f.paths).HeldSerial()
		if rec.State != StateAdopted || rec.Serial != serial || rec.Sequence != 0 {
			t.Fatalf("journal after restore: %+v (held %s)", rec, serial)
		}
	})
	t.Run("damaged staged key before any rename restores the parent", func(t *testing.T) {
		f := newFixture(t, ca, v2Leaf, 0o600)
		adoptCrashState(t, f, "committed")
		damage(t, f.paths.Key+".next")
		if err := Open(f.paths).Recover(); !errors.Is(err, ErrRecoveredToParent) {
			t.Fatalf("want ErrRecoveredToParent, got %v", err)
		}
		assertLive(t, f.paths, f.parent, "restored")
	})
	t.Run("damaged previous files too give ErrStoreDamaged", func(t *testing.T) {
		f := newFixture(t, ca, v2Leaf, 0o600)
		adoptCrashState(t, f, "renamed-key")
		damage(t, f.paths.Cert+".next")
		damage(t, f.paths.Key+".prev")
		snap := snapshot(t, f.dir)
		err := Open(f.paths).Recover()
		if !errors.Is(err, ErrStoreDamaged) {
			t.Fatalf("want ErrStoreDamaged, got %v", err)
		}
		if snapshot(t, f.dir) != snap {
			t.Fatal("Recover changed the directory of a damaged store")
		}
	})
}

func TestRecoverSuperseded(t *testing.T) {
	ca := csrtest.NewCA(t)
	other := csrtest.NewCA(t)
	for _, point := range []string{"committed", "renamed-ca", "checked", "prepared"} {
		t.Run(point, func(t *testing.T) {
			f := newFixture(t, ca, v2Leaf, 0o600)
			adoptCrashState(t, f, point)
			// A new registration writes foreign live files.
			foreign := material(t, other, csrtest.LeafOptions{DNSNames: []string{"acct-c.example.test"}, Version: 2, Type: 1})
			writeLive(t, f.paths, foreign, 0o600)
			err := Open(f.paths).Recover()
			if !errors.Is(err, ErrAdoptionSuperseded) {
				t.Fatalf("want ErrAdoptionSuperseded, got %v", err)
			}
			if !liveIs(t, f.paths, foreign) {
				t.Fatal("live files changed")
			}
			if s := strayFiles(t, f.dir); len(s) != 0 {
				t.Fatalf("stray files remain: %v", s)
			}
			if err := Open(f.paths).Recover(); err != nil {
				t.Fatalf("second Recover: %v", err)
			}
			if !liveIs(t, f.paths, foreign) {
				t.Fatal("second Recover changed live files")
			}
		})
	}
}

func TestRecoverQuietWithoutState(t *testing.T) {
	ca := csrtest.NewCA(t)
	cases := map[string]func(t *testing.T) Paths{
		"registered": func(t *testing.T) Paths { return newFixture(t, ca, v2Leaf, 0o600).paths },
		"missing credential files": func(t *testing.T) Paths {
			d := t.TempDir()
			return Paths{Cert: d + "/mtls.crt", Key: d + "/mtls.key", CA: d + "/ca.crt"}
		},
		"empty credential files": func(t *testing.T) Paths {
			d := t.TempDir()
			p := Paths{Cert: d + "/mtls.crt", Key: d + "/mtls.key", CA: d + "/ca.crt"}
			writeLive(t, p, Material{}, 0o600)
			return p
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			p := mk(t)
			dir := strings.TrimSuffix(p.Cert, "/mtls.crt")
			snap := snapshot(t, dir)
			buf := captureLog(t)
			if err := Open(p).Recover(); err != nil {
				t.Fatalf("Recover: %v", err)
			}
			if snapshot(t, dir) != snap {
				t.Fatal("Recover changed the directory")
			}
			if buf.Len() != 0 {
				t.Fatalf("Recover wrote a log line: %s", buf.String())
			}
		})
	}
	t.Run("adopted journal and no stray file", func(t *testing.T) {
		f := newFixture(t, ca, v2Leaf, 0o600)
		if err := prepareAndAdopt(Open(f.paths), f.child); err != nil {
			t.Fatal(err)
		}
		snap := snapshot(t, f.dir)
		buf := captureLog(t)
		if err := Open(f.paths).Recover(); err != nil {
			t.Fatal(err)
		}
		if snapshot(t, f.dir) != snap || buf.Len() != 0 {
			t.Fatalf("Recover was not quiet: %s", buf.String())
		}
	})
}

func TestChildRejectedKeepsParent(t *testing.T) {
	ca := csrtest.NewCA(t)
	other := csrtest.NewCA(t)
	now := time.Now()
	cases := []struct {
		reason string
		child  func(f *fixture) Material
	}{
		{"key_mismatch", func(f *fixture) Material {
			m := material(t, ca, v2Leaf)
			_, k := newECKey(t)
			m.KeyPEM = k
			return m
		}},
		{"chain_invalid", func(f *fixture) Material {
			m := material(t, other, v2Leaf)
			m.CAPEM = []byte(ca.PEM)
			return m
		}},
		{"account_mismatch", func(f *fixture) Material {
			return material(t, ca, csrtest.LeafOptions{DNSNames: []string{"acct-b.example.test"}, Version: 2, Type: 1})
		}},
		{"type_mismatch", func(f *fixture) Material {
			return material(t, ca, csrtest.LeafOptions{DNSNames: v2Leaf.DNSNames, Version: 2, Type: 2})
		}},
		{"type_mismatch", func(f *fixture) Material {
			return material(t, ca, csrtest.LeafOptions{DNSNames: v2Leaf.DNSNames, Version: 2})
		}},
		{"version_mismatch", func(f *fixture) Material {
			return material(t, ca, csrtest.LeafOptions{DNSNames: v2Leaf.DNSNames, Version: 3, Type: 1})
		}},
		{"expired", func(f *fixture) Material {
			return material(t, ca, csrtest.LeafOptions{DNSNames: v2Leaf.DNSNames, Version: 2, Type: 1,
				NotBefore: now.Add(-48 * time.Hour), NotAfter: now.Add(-time.Hour)})
		}},
		{"no_client_auth", func(f *fixture) Material {
			return material(t, ca, csrtest.LeafOptions{DNSNames: v2Leaf.DNSNames, Version: 2, Type: 1, NoClientAuth: true})
		}},
		{"malformed", func(f *fixture) Material {
			m := material(t, ca, v2Leaf)
			m.CertPEM = append(append([]byte{}, m.CertPEM...), m.CertPEM...)
			return m
		}},
		{"malformed", func(f *fixture) Material {
			m := material(t, ca, v2Leaf)
			m.KeyPEM = []byte("not a key")
			return m
		}},
	}
	for _, c := range cases {
		t.Run(c.reason, func(t *testing.T) {
			f := newFixture(t, ca, v2Leaf, 0o600)
			s := Open(f.paths)
			if err := s.Prepare(); err != nil {
				t.Fatal(err)
			}
			err := s.Adopt(c.child(f))
			if !errors.Is(err, ErrChildRejected) {
				t.Fatalf("want ErrChildRejected, got %v", err)
			}
			if got := RejectReason(err); got != c.reason {
				t.Fatalf("reason %q, want %q (%v)", got, c.reason, err)
			}
			if strings.Contains(err.Error(), "-----BEGIN") {
				t.Fatal("rejection text carries PEM data")
			}
			assertLive(t, f.paths, f.parent, "after rejection")
			for _, p := range []string{f.paths.CA, f.paths.Key, f.paths.Cert} {
				if _, err := os.Stat(p + ".next"); !os.IsNotExist(err) {
					t.Fatalf("%s.next remains", p)
				}
			}
		})
	}
	t.Run("v1 parent accepts the values of the child", func(t *testing.T) {
		f := newFixture(t, ca, csrtest.LeafOptions{}, 0o600)
		if err := prepareAndAdopt(Open(f.paths), f.child); err != nil {
			t.Fatalf("adopt under a v1 parent: %v", err)
		}
		assertLive(t, f.paths, f.child, "v1 parent")
	})
}

func TestFileModes(t *testing.T) {
	ca := csrtest.NewCA(t)
	tmpModes := map[string]os.FileMode{}
	watchTemps := func(s *Store) *Store {
		s.afterTemp = func(path string) {
			tmpModes[path] = modeOf(t, path)
		}
		return s
	}
	checkJournal := func(t *testing.T, p Paths, at string) {
		b, err := os.ReadFile(journalPath(p))
		if err != nil {
			return
		}
		if strings.Contains(string(b), "PRIVATE KEY") || strings.Contains(string(b), "-----BEGIN") {
			t.Fatalf("journal holds PEM material at %s", at)
		}
		if m := modeOf(t, journalPath(p)); m != 0o644 {
			t.Fatalf("journal mode %o at %s", m, at)
		}
	}
	checkTemps := func(t *testing.T) {
		for path, m := range tmpModes {
			if strings.Contains(path, ".key.") && m != 0o600 {
				t.Fatalf("temporary key file %s has mode %o", path, m)
			}
		}
	}

	t.Run("prepared from a legacy 0644 key", func(t *testing.T) {
		f := newFixture(t, ca, v2Leaf, 0o644)
		s := watchTemps(crashAt(f.paths, "prepared", nil))
		s.hook = func(at string) error {
			checkJournal(t, f.paths, at)
			if at == "prepared" {
				return errCrash
			}
			return nil
		}
		if err := s.Prepare(); !errors.Is(err, errCrash) {
			t.Fatal(err)
		}
		if m := modeOf(t, f.paths.Key+".prev"); m != 0o600 {
			t.Fatalf(".prev key mode %o, want 600", m)
		}
		if m := modeOf(t, f.paths.Cert+".prev"); m != 0o644 {
			t.Fatalf(".prev cert mode %o", m)
		}
		checkTemps(t)
	})
	t.Run("adoption and roll forward", func(t *testing.T) {
		f := newFixture(t, ca, v2Leaf, 0o644)
		s := watchTemps(Open(f.paths))
		s.hook = func(at string) error {
			checkJournal(t, f.paths, at)
			if at == "staged-cert" {
				return errCrash
			}
			return nil
		}
		if err := prepareAndAdopt(s, f.child); !errors.Is(err, errCrash) {
			t.Fatal(err)
		}
		if m := modeOf(t, f.paths.Key+".next"); m != 0o600 {
			t.Fatalf(".next key mode %o", m)
		}
		r := watchTemps(Open(f.paths))
		r.hook = func(at string) error { checkJournal(t, f.paths, at); return nil }
		if err := r.Recover(); err != nil {
			t.Fatal(err)
		}
		if m := modeOf(t, f.paths.Key); m != 0o600 {
			t.Fatalf("adopted key mode %o", m)
		}
		if modeOf(t, f.paths.Cert) != 0o644 || modeOf(t, f.paths.CA) != 0o644 {
			t.Fatal("certificate or CA mode is not 644")
		}
		checkJournal(t, f.paths, "end")
		checkTemps(t)
	})
	t.Run("restore after a crash", func(t *testing.T) {
		f := newFixture(t, ca, v2Leaf, 0o644)
		adoptCrashState(t, f, "renamed-key")
		damage(t, f.paths.Cert+".next")
		if err := watchTemps(Open(f.paths)).Recover(); !errors.Is(err, ErrRecoveredToParent) {
			t.Fatal(err)
		}
		if m := modeOf(t, f.paths.Key); m != 0o600 {
			t.Fatalf("restored key mode %o, want 600", m)
		}
		checkJournal(t, f.paths, "restored")
		checkTemps(t)
	})
	if len(tmpModes) == 0 {
		t.Fatal("no temporary file was observed")
	}
}

func TestAdoptNeedsPrepare(t *testing.T) {
	ca := csrtest.NewCA(t)
	f := newFixture(t, ca, v2Leaf, 0o600)
	snap := snapshot(t, f.dir)
	if err := Open(f.paths).Adopt(f.child); !errors.Is(err, ErrNotPrepared) {
		t.Fatalf("want ErrNotPrepared, got %v", err)
	}
	if snapshot(t, f.dir) != snap {
		t.Fatal("Adopt without Prepare wrote to the directory")
	}
}

func TestPrepareRefusesUnusableParent(t *testing.T) {
	ca := csrtest.NewCA(t)
	f := newFixture(t, ca, v2Leaf, 0o600)
	writeLive(t, f.paths, Material{}, 0o600)
	snap := snapshot(t, f.dir)
	if err := Open(f.paths).Prepare(); err == nil {
		t.Fatal("Prepare accepted empty credential files")
	}
	if snapshot(t, f.dir) != snap {
		t.Fatal("Prepare wrote to the directory")
	}
}

func TestAdoptLegacyRSAPair(t *testing.T) {
	ca := csrtest.NewCA(t)
	f := newFixture(t, ca, v2Leaf, 0o600)
	k, keyPEM := newRSAKey(t)
	child := Material{KeyPEM: keyPEM, CertPEM: []byte(ca.LeafWith(t, k.Public(), v2Leaf)), CAPEM: []byte(ca.PEM)}
	if err := prepareAndAdopt(Open(f.paths), child); err != nil {
		t.Fatalf("adopt RSA pair: %v", err)
	}
	assertLive(t, f.paths, child, "RSA child")
	if s := strayFiles(t, f.dir); len(s) != 0 {
		t.Fatalf("stray files remain: %v", s)
	}
}

func TestAdoptedAndHeldSerial(t *testing.T) {
	ca := csrtest.NewCA(t)
	f := newFixture(t, ca, v2Leaf, 0o600)
	if _, ok, err := Open(f.paths).Adopted(); ok || err != nil {
		t.Fatalf("no journal: ok=%v err=%v", ok, err)
	}
	if err := prepareAndAdopt(Open(f.paths), f.child); err != nil {
		t.Fatal(err)
	}
	rec, ok, err := Open(f.paths).Adopted()
	if !ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	held, err := Open(f.paths).HeldSerial()
	if err != nil {
		t.Fatal(err)
	}
	if rec.Serial != held || held != strings.ToLower(held) || rec.Sequence != 1 || rec.NotAfter.IsZero() || rec.AdoptedAt.IsZero() {
		t.Fatalf("record %+v, held %s", rec, held)
	}
	// A second adoption raises the sequence again.
	second := material(t, ca, v2Leaf)
	if err := prepareAndAdopt(Open(f.paths), second); err != nil {
		t.Fatal(err)
	}
	rec, _, _ = Open(f.paths).Adopted()
	if rec.Sequence != 2 {
		t.Fatalf("sequence %d, want 2", rec.Sequence)
	}
}

func TestCorruptJournalChangesNothing(t *testing.T) {
	ca := csrtest.NewCA(t)
	f := newFixture(t, ca, v2Leaf, 0o600)
	if err := os.WriteFile(journalPath(f.paths), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	snap := snapshot(t, f.dir)
	if err := Open(f.paths).Recover(); !errors.Is(err, ErrStoreDamaged) {
		t.Fatalf("want ErrStoreDamaged, got %v", err)
	}
	if snapshot(t, f.dir) != snap {
		t.Fatal("Recover changed the directory")
	}
}
