package commons

import (
	"os"
	"path/filepath"
	"testing"
)

// The agent restores nodeward's wg-quick instance unit on start, so a node installed before the
// ordering reset is repaired without a reinstall. These pin the write rules; the content itself
// is pinned against nodeward's copy by a source guard in that repo.

func TestUnitIsWrittenWhenAbsentOrDifferent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "override.conf")

	changed, err := ensureFileContent(path, WgQuickUnit)
	if err != nil || !changed {
		t.Fatalf("absent file must be written: changed=%v err=%v", changed, err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != WgQuickUnit {
		t.Fatalf("wrote different bytes:\n%s", got)
	}

	// The old, appended-style file a pre-reset install left behind.
	os.WriteFile(path, []byte("[Unit]\nAfter=network-online.target\nWants=network-online.target\n"), 0o644)
	changed, err = ensureFileContent(path, WgQuickUnit)
	if err != nil || !changed {
		t.Fatalf("a differing file must be rewritten: changed=%v err=%v", changed, err)
	}

	changed, err = ensureFileContent(path, WgQuickUnit)
	if err != nil || changed {
		t.Fatalf("a matching file must be left alone: changed=%v err=%v", changed, err)
	}
}

func TestUnitIsNotCreatedWhereTheDirectoryIsMissing(t *testing.T) {
	// No drop-in directory means no wg0 unit yet (mid-install, or not a RunOS node at all).
	path := filepath.Join(t.TempDir(), "wg-quick@wg0.service.d", "override.conf")
	changed, err := ensureFileContent(path, WgQuickUnit)
	if err != nil || changed {
		t.Fatalf("must not create the drop-in directory: changed=%v err=%v", changed, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("file was created under a directory that did not exist")
	}
}

func TestUnitDoesNotOrderAfterNameResolution(t *testing.T) {
	// nss-lookup.target is the cycle; the unit is the stock template without it.
	for _, l := range splitLines(WgQuickUnit) {
		if len(l) > 0 && l[0] == '#' {
			continue
		}
		if contains(l, "nss-lookup") {
			t.Fatalf("the wg0 unit must not depend on nss-lookup.target: %q", l)
		}
	}
	for _, want := range []string{"\nAfter=network-online.target\n", "ExecStart=/usr/bin/wg-quick up wg0\n", "WantedBy=multi-user.target\n"} {
		if !contains(WgQuickUnit, want) {
			t.Fatalf("unit lacks %q", want)
		}
	}
}

func splitLines(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == '\n' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	return append(out, cur)
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
