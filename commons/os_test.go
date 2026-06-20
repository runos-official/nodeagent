package commons

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseOSRelease(t *testing.T) {
	cases := []struct {
		name        string
		in          string
		wantID      string
		wantIDLike  string
		wantVersion string
		wantUbuntu  bool
	}{
		{
			name:        "ubuntu 24.04",
			in:          "NAME=\"Ubuntu\"\nID=ubuntu\nID_LIKE=debian\nVERSION_ID=\"24.04\"\n",
			wantID:      "ubuntu",
			wantIDLike:  "debian",
			wantVersion: "24.04",
			wantUbuntu:  true,
		},
		{
			name:        "ubuntu 26.04",
			in:          "ID=ubuntu\nVERSION_ID=\"26.04\"\nNAME=\"Ubuntu 26.04 LTS\"\n",
			wantID:      "ubuntu",
			wantVersion: "26.04",
			wantUbuntu:  true,
		},
		{
			name:        "derivative via ID_LIKE (Pop!_OS)",
			in:          "NAME=\"Pop!_OS\"\nID=pop\nID_LIKE=\"ubuntu debian\"\nVERSION_ID=\"22.04\"\n",
			wantID:      "pop",
			wantIDLike:  "ubuntu debian",
			wantVersion: "22.04",
			wantUbuntu:  true,
		},
		{
			name:        "non-ubuntu (rhel)",
			in:          "NAME=\"Red Hat Enterprise Linux\"\nID=\"rhel\"\nID_LIKE=\"fedora\"\nVERSION_ID=\"9.3\"\n",
			wantID:      "rhel",
			wantIDLike:  "fedora",
			wantVersion: "9.3",
			wantUbuntu:  false,
		},
		{
			name:       "comments and blank lines ignored",
			in:         "# a comment\n\nID=ubuntu\n  VERSION_ID = \"24.04\"  \n",
			wantID:     "ubuntu",
			wantUbuntu: true,
			// VERSION_ID parsing tolerates surrounding whitespace on the value.
			wantVersion: "24.04",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rel := parseOSRelease(strings.NewReader(tc.in))
			if rel.ID != tc.wantID {
				t.Errorf("ID = %q, want %q", rel.ID, tc.wantID)
			}
			if rel.IDLike != tc.wantIDLike {
				t.Errorf("IDLike = %q, want %q", rel.IDLike, tc.wantIDLike)
			}
			if rel.VersionID != tc.wantVersion {
				t.Errorf("VersionID = %q, want %q", rel.VersionID, tc.wantVersion)
			}
			if got := rel.IsUbuntu(); got != tc.wantUbuntu {
				t.Errorf("IsUbuntu() = %v, want %v", got, tc.wantUbuntu)
			}
		})
	}
}

func TestReadOSReleaseAndGetOSInfo(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "os-release")
	if err := os.WriteFile(path, []byte("ID=ubuntu\nVERSION_ID=\"26.04\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	orig := osReleasePath
	osReleasePath = path
	defer func() { osReleasePath = orig }()

	rel, err := ReadOSRelease()
	if err != nil {
		t.Fatalf("ReadOSRelease() error: %v", err)
	}
	if rel.ID != "ubuntu" || rel.VersionID != "26.04" {
		t.Fatalf("ReadOSRelease() = %+v, want ubuntu/26.04", rel)
	}
	if got := GetOSInfo(); got != "ubuntu-26.04" {
		t.Fatalf("GetOSInfo() = %q, want ubuntu-26.04", got)
	}
}

func TestGetOSInfoUnknownOnMissingFile(t *testing.T) {
	orig := osReleasePath
	osReleasePath = filepath.Join(t.TempDir(), "does-not-exist")
	defer func() { osReleasePath = orig }()

	if got := GetOSInfo(); got != "unknown" {
		t.Fatalf("GetOSInfo() = %q, want unknown", got)
	}
}
