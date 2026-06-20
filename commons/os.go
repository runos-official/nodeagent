package commons

import (
	"bufio"
	"io"
	"os"
	"strings"
)

// osReleasePath is the standard location of the os-release metadata. It is a
// package var so tests can point the parser at a fixture file.
var osReleasePath = "/etc/os-release"

// OSRelease holds the fields parsed from /etc/os-release that the agent cares
// about. ID/IDLike/VersionID come straight from the file (quotes stripped).
type OSRelease struct {
	ID        string // e.g. "ubuntu"
	IDLike    string // e.g. "debian" (or "ubuntu" on derivatives)
	VersionID string // e.g. "24.04"
	Name      string // e.g. "Ubuntu 24.04.1 LTS"
}

// IsUbuntu reports whether this is Ubuntu or an Ubuntu derivative, gating on
// ID=ubuntu or ID_LIKE containing "ubuntu".
func (o OSRelease) IsUbuntu() bool {
	if strings.EqualFold(o.ID, "ubuntu") {
		return true
	}
	return strings.Contains(strings.ToLower(o.IDLike), "ubuntu")
}

// parseOSRelease parses the key=value lines of an os-release stream. Values may
// be optionally quoted; we strip surrounding double quotes. Unknown keys are
// ignored. This is the single os-release parser shared by preflight and
// GetOSInfo so their verdicts can never diverge (audit HIGH-2).
func parseOSRelease(r io.Reader) OSRelease {
	var rel OSRelease
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), "\"")
		switch strings.TrimSpace(key) {
		case "ID":
			rel.ID = value
		case "ID_LIKE":
			rel.IDLike = value
		case "VERSION_ID":
			rel.VersionID = value
		case "NAME":
			rel.Name = value
		}
	}
	return rel
}

// ReadOSRelease reads and parses /etc/os-release. On read failure it returns an
// empty OSRelease and the error so callers can decide how to surface it.
func ReadOSRelease() (OSRelease, error) {
	file, err := os.Open(osReleasePath)
	if err != nil {
		return OSRelease{}, err
	}
	defer file.Close()
	return parseOSRelease(file), nil
}

// GetOSInfo returns the OS name and version in format "name-version" (e.g., "ubuntu-24.04").
// Returns "unknown" if /etc/os-release is unreadable or has no ID.
func GetOSInfo() string {
	rel, err := ReadOSRelease()
	if err != nil || rel.ID == "" {
		return "unknown"
	}
	if rel.VersionID != "" {
		return rel.ID + "-" + rel.VersionID
	}
	return rel.ID
}
