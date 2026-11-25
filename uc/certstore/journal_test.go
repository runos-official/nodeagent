package certstore

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestJournalRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, JournalName)
	if _, ok, err := readJournal(path); ok || err != nil {
		t.Fatalf("missing journal: ok=%v err=%v", ok, err)
	}
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, rec := range []Record{
		{State: StatePrepared, Prev: Digests{CA: "a", Key: "b", Cert: "c"}, ParentSerial: "01", UpdatedAt: at},
		{State: StateCommitting, Prev: Digests{CA: "a", Key: "b", Cert: "c"}, Next: Digests{CA: "d", Key: "e", Cert: "f"},
			ParentSerial: "01", ChildSerial: "02", ChildNotAfter: at, UpdatedAt: at},
		{State: StateAdopted, Sequence: 3, Serial: "02", NotAfter: at, AdoptedAt: at, UpdatedAt: at},
	} {
		if err := writeJournal(path, rec, nil); err != nil {
			t.Fatal(err)
		}
		got, ok, err := readJournal(path)
		if err != nil || !ok {
			t.Fatalf("read: ok=%v err=%v", ok, err)
		}
		if got != rec {
			t.Fatalf("round trip:\n got %+v\nwant %+v", got, rec)
		}
		if m := modeOf(t, path); m != 0o644 {
			t.Fatalf("journal mode %o", m)
		}
		if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
			t.Fatal("temporary journal file remains")
		}
	}
	if err := os.WriteFile(path, []byte(`{"state":"other"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readJournal(path); err == nil {
		t.Fatal("an unknown state must not read as valid")
	}
}

func TestWriteFileSync(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.key.next")
	var seen string
	if err := writeFileSync(path, []byte("data"), 0o600, func(p string) { seen = p }); err != nil {
		t.Fatal(err)
	}
	if seen != path+".tmp" {
		t.Fatalf("temp hook saw %q", seen)
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "data" {
		t.Fatalf("read %q %v", b, err)
	}
	if m := modeOf(t, path); m != 0o600 {
		t.Fatalf("mode %o", m)
	}
	// An existing file is replaced, and its mode follows the request.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeFileSync(path, []byte("new"), 0o600, nil); err != nil {
		t.Fatal(err)
	}
	if m := modeOf(t, path); m != 0o600 {
		t.Fatalf("mode after replace %o", m)
	}
	if err := syncDir(dir); err != nil {
		t.Fatal(err)
	}
}
