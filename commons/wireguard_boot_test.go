package commons

import (
	"os"
	"path/filepath"
	"testing"
)

// The agent restores nodeward's wg-quick override on start, so a node installed before the
// ordering reset is repaired without a reinstall. These pin the write rules; the content itself
// is pinned against nodeward's copy by a source guard in that repo.

func TestOverrideIsWrittenWhenAbsentOrDifferent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "override.conf")

	changed, err := ensureFileContent(path, WgQuickOverride)
	if err != nil || !changed {
		t.Fatalf("absent file must be written: changed=%v err=%v", changed, err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != WgQuickOverride {
		t.Fatalf("wrote different bytes:\n%s", got)
	}

	// The old, appended-style file a pre-reset install left behind.
	os.WriteFile(path, []byte("[Unit]\nAfter=network-online.target\nWants=network-online.target\n"), 0o644)
	changed, err = ensureFileContent(path, WgQuickOverride)
	if err != nil || !changed {
		t.Fatalf("a differing file must be rewritten: changed=%v err=%v", changed, err)
	}

	changed, err = ensureFileContent(path, WgQuickOverride)
	if err != nil || changed {
		t.Fatalf("a matching file must be left alone: changed=%v err=%v", changed, err)
	}
}

func TestOverrideIsNotCreatedWhereTheUnitIsNotInstalled(t *testing.T) {
	// No drop-in directory means no wg0 unit yet (mid-install, or not a RunOS node at all).
	path := filepath.Join(t.TempDir(), "wg-quick@wg0.service.d", "override.conf")
	changed, err := ensureFileContent(path, WgQuickOverride)
	if err != nil || changed {
		t.Fatalf("must not create the drop-in directory: changed=%v err=%v", changed, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("file was created under a directory that did not exist")
	}
}

func TestOverrideResetsTheStockOrdering(t *testing.T) {
	// The reset lines are the fix; without them the stock After=nss-lookup.target survives.
	for _, want := range []string{"\nAfter=\n", "\nAfter=network-online.target\n", "\nWants=\n"} {
		if !contains(WgQuickOverride, want) {
			t.Fatalf("override lacks %q", want)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
