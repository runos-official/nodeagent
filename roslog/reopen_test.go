package roslog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// After logrotate renames the log file, a reopen must send new lines to a new
// file at the same path and leave the renamed file alone.
func TestReopenWriter_WritesToNewFileAfterRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runos.log")

	w, err := newReopenWriter(path)
	if err != nil {
		t.Fatalf("newReopenWriter: %v", err)
	}
	defer w.Close()

	if _, err := w.Write([]byte("before\n")); err != nil {
		t.Fatalf("write before: %v", err)
	}

	rotated := path + ".1"
	if err := os.Rename(path, rotated); err != nil {
		t.Fatalf("rename: %v", err)
	}
	// Without a reopen the writer still holds the renamed file.
	if err := w.Reopen(); err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	if _, err := w.Write([]byte("after\n")); err != nil {
		t.Fatalf("write after: %v", err)
	}

	oldData, _ := os.ReadFile(rotated)
	newData, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read new file: %v", err)
	}
	if string(oldData) != "before\n" {
		t.Fatalf("rotated file = %q, want only the line written before rotation", oldData)
	}
	if string(newData) != "after\n" {
		t.Fatalf("new file = %q, want only the line written after reopen", newData)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatalf("new file mode = %v, want 0600", info.Mode().Perm())
	}
}

// A failed reopen must keep the old file so no log line is lost.
func TestReopenWriter_FailedReopenKeepsOldFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runos.log")
	w, err := newReopenWriter(path)
	if err != nil {
		t.Fatalf("newReopenWriter: %v", err)
	}
	defer w.Close()

	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	// Put a directory at the path so the open fails.
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := w.Reopen(); err == nil {
		t.Fatal("Reopen succeeded, want an error")
	}
	if _, err := w.Write([]byte("still works\n")); err != nil {
		t.Fatalf("write after failed reopen: %v", err)
	}
	data, _ := os.ReadFile(path + ".1")
	if !strings.Contains(string(data), "still works") {
		t.Fatalf("old file = %q, want the line written after the failed reopen", data)
	}
}
