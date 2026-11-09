package agentstream

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestMovePatchFactsMarkerDoesNotReplaceExistingEntry(t *testing.T) {
	dir := t.TempDir()
	pending := filepath.Join(dir, "pending")
	processing := filepath.Join(dir, "processing")
	if err := os.WriteFile(pending, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(processing, []byte("already claimed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := movePatchFactsMarkerNoReplace(pending, processing); !errors.Is(err, syscall.EEXIST) {
		t.Fatalf("claim with an existing destination returned %v, want EEXIST", err)
	}
	assertContents := func(path, want string) {
		t.Helper()
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatalf("read %s = %q, %v, want %q", filepath.Base(path), got, err, want)
		}
	}
	assertContents(pending, "new")
	assertContents(processing, "already claimed")
	if err := os.Remove(processing); err != nil {
		t.Fatal(err)
	}
	if err := movePatchFactsMarkerNoReplace(pending, processing); err != nil {
		t.Fatalf("claim with no existing destination: %v", err)
	}
	assertContents(processing, "new")
	if _, err := os.Lstat(pending); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pending marker should be consumed, got %v", err)
	}
}
