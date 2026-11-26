package certificate

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRenewalTimingHelper(t *testing.T) {
	notBefore := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	notAfter := notBefore.Add(90 * 24 * time.Hour)
	const nodeID = "00000000-0000-4000-8000-0000000000aa"
	due := renewalTiming(notBefore, notAfter, nodeID, notBefore).DueAt

	if lead := notAfter.Sub(due); lead < 90*24*time.Hour/4 || lead > 30*24*time.Hour {
		t.Fatalf("due %v before NotAfter, want between 22.5 and 30 days", lead)
	}
	if renewalTiming(notBefore, notAfter, nodeID, due.Add(-time.Second)).Due {
		t.Fatal("due one second before the due point")
	}
	if !renewalTiming(notBefore, notAfter, nodeID, due).Due {
		t.Fatal("not due at the due point")
	}
}

// TestOldThresholdGone keeps the fixed day threshold out of the repository.
// The due point follows the certificate's own life (certpolicy.Evaluate).
func TestOldThresholdGone(t *testing.T) {
	name := "RenewalThreshold" + "Days"
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repository root not found at %s: %v", root, err)
	}
	scanned := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		scanned++
		if strings.Contains(string(src), name) {
			t.Errorf("%s still names %s", path, name)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned < 50 {
		t.Fatalf("scanned only %d Go files: wrong root", scanned)
	}
}
