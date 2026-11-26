package certificate

import (
	"strings"
	"testing"
)

// TestRenewHelpNamesNoBackup: the help of `runos certificate renew` names
// the staged adoption, not a backup or a restore.
func TestRenewHelpNamesNoBackup(t *testing.T) {
	text := strings.ToLower(renewCmd.Short + "\n" + renewCmd.Long + "\n" + renewCmd.Example)
	for _, w := range []string{"back up", "backup", "restor"} {
		if strings.Contains(text, w) {
			t.Fatalf("help names %q", w)
		}
	}
	if !strings.Contains(text, "adopt it in stages") {
		t.Fatal("help does not describe the staged adoption")
	}
}
