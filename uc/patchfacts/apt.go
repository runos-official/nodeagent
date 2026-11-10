package patchfacts

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
)

// parseLines returns the non-empty trimmed lines of out. apt-mark showhold and
// the reboot trigger list are both one name per line.
func parseLines(out string) []string {
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			lines = append(lines, s)
		}
	}
	return lines
}

// parseBootTime reads the btime line of /proc/stat, which carries the boot time
// as a count of seconds since the epoch.
func parseBootTime(procStat string) (time.Time, error) {
	for _, line := range strings.Split(procStat, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "btime" {
			continue
		}
		secs, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return time.Time{}, fmt.Errorf("the btime line of %s does not carry a number: %q", pathProcStat, fields[1])
		}
		return time.Unix(secs, 0).UTC(), nil
	}
	return time.Time{}, fmt.Errorf("%s carries no btime line", pathProcStat)
}

// parseDpkgVersions reads rows of "<package> <version> <status>" and keeps the
// installed ones. dpkg-query prints a row for a package it knows but has not
// installed, and such a row carries an EMPTY version, so the status column
// decides.
func parseDpkgVersions(out string) map[string]string {
	versions := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 || fields[2] != "installed" {
			continue
		}
		versions[fields[0]] = fields[1]
	}
	return versions
}

// kernelRelease returns the kernel release a linux-image package name carries,
// for example "6.8.0-147-generic" for "linux-image-6.8.0-147-generic". It
// returns false for a meta package such as linux-image-generic or
// linux-image-virtual, and for the unsigned variant, because neither names one
// installed kernel.
//
// The release, and not the package version, is the value the record carries:
// the record's running kernel comes from uname -r in exactly this vocabulary,
// and a reader compares the two.
func kernelRelease(pkg string) (string, bool) {
	const prefix = "linux-image-"
	if !strings.HasPrefix(pkg, prefix) {
		return "", false
	}
	rest := strings.TrimPrefix(pkg, prefix)
	if rest == "" || rest[0] < '0' || rest[0] > '9' {
		// A meta package (linux-image-generic) or a variant
		// (linux-image-unsigned-...) does not start with a version.
		return "", false
	}
	return rest, true
}

// parseInstalledKernels returns the installed kernel releases, newest first.
// The order comes from the Debian comparison over the package versions, so
// 6.8.0-147.147 sorts above 6.8.0-60.63.
func parseInstalledKernels(out string) []string {
	type kernel struct {
		release string
		version string
	}
	var kernels []kernel
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 || fields[2] != "installed" {
			continue
		}
		release, ok := kernelRelease(fields[0])
		if !ok {
			continue
		}
		kernels = append(kernels, kernel{release: release, version: fields[1]})
	}
	sort.SliceStable(kernels, func(i, j int) bool {
		return compareDebianVersions(kernels[i].version, kernels[j].version) > 0
	})
	releases := make([]string, 0, len(kernels))
	for _, k := range kernels {
		releases = append(releases, k.release)
	}
	return releases
}

// waitingUpdate is one package apt would upgrade, with the version installed
// now.
type waitingUpdate struct {
	name             string
	installedVersion string
}

// parseSimulatedUpgrade reads the Inst lines of `apt-get -s upgrade`.
//
// A line has the form, measured on a running Ubuntu 24.04 node:
//
//	Inst systemd [255.4-1ubuntu8.17] (255.4-1ubuntu8.18 Ubuntu:24.04/noble-updates, Ubuntu:24.04/noble-security [amd64])
//
// The bracket after the name is the version installed now. A line with NO such
// bracket is a package apt would newly install, not a waiting update for an
// installed package, so it is skipped. `apt-get upgrade` never prints one, and
// skipping it keeps the reading right if a caller ever passes a different
// apt verb.
func parseSimulatedUpgrade(out string) []waitingUpdate {
	var updates []waitingUpdate
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "Inst ") {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(line, "Inst "))
		name, rest, ok := strings.Cut(rest, " ")
		if !ok || name == "" {
			continue
		}
		rest = strings.TrimSpace(rest)
		if !strings.HasPrefix(rest, "[") {
			continue
		}
		installed, _, ok := strings.Cut(strings.TrimPrefix(rest, "["), "]")
		if !ok {
			continue
		}
		updates = append(updates, waitingUpdate{name: name, installedVersion: strings.TrimSpace(installed)})
	}
	return updates
}

// packageIndex is one package index, with the origin and archive of its own
// Release file.
type packageIndex struct {
	origin  string
	archive string
}

// parsePolicyIndexes reads `apt-cache policy` with no arguments and maps each
// index's describe line to the origin and archive of that index's Release file.
//
// The output has the form, measured on a running Ubuntu 24.04 node:
//
//	500 http://security.ubuntu.com/ubuntu noble-security/main amd64 Packages
//	    release v=24.04,o=Ubuntu,a=noble-security,n=noble,l=Ubuntu,c=main,b=amd64
//	    origin security.ubuntu.com
//
// The describe line is the key, because the version table of the per-package
// output repeats exactly that text. Reading o= and a= from the release line is
// what makes a security pocket a fact of the Release file rather than of the
// spelling in sources.list.
func parsePolicyIndexes(out string) map[string]packageIndex {
	indexes := map[string]packageIndex{}
	var current string
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "" || trimmed == "Package files:" || trimmed == "Pinned packages:":
			continue
		case strings.HasPrefix(trimmed, "release "):
			if current == "" {
				continue
			}
			idx := packageIndex{}
			for _, pair := range strings.Split(strings.TrimPrefix(trimmed, "release "), ",") {
				key, value, ok := strings.Cut(pair, "=")
				if !ok {
					continue
				}
				switch key {
				case "o":
					idx.origin = value
				case "a":
					idx.archive = value
				}
			}
			indexes[current] = idx
		case strings.HasPrefix(trimmed, "origin "):
			continue
		default:
			// A priority and then the describe text.
			priority, describe, ok := strings.Cut(trimmed, " ")
			if !ok {
				continue
			}
			if _, err := strconv.Atoi(priority); err != nil {
				continue
			}
			current = strings.TrimSpace(describe)
		}
	}
	return indexes
}

// availableVersion is one version of one package, with the describe lines of
// the indexes that carry it.
type availableVersion struct {
	version  string
	describe []string
}

// parsePolicyPackages reads `apt-cache policy <names>` and returns, per
// package, every available version with the indexes that carry it.
//
// The output has the form, measured on a running Ubuntu 24.04 node:
//
//	openssl:
//	  Installed: 3.0.22-1~deb12u2
//	  Candidate: 3.0.22-1~deb12u2
//	  Version table:
//	 *** 3.0.22-1~deb12u2 500
//	        500 http://deb.debian.org/debian-security bookworm-security/main arm64 Packages
//	        100 /var/lib/dpkg/status
//	     3.0.20-1~deb12u2 500
//	        500 http://deb.debian.org/debian bookworm/main arm64 Packages
//
// apt writes a version row with " *** " when that version is installed and with
// five spaces otherwise, and an index row with eight spaces. The eight-space
// test comes first, because an index row also starts with five spaces.
func parsePolicyPackages(out string) map[string][]availableVersion {
	packages := map[string][]availableVersion{}
	var name string
	var inTable bool
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		switch {
		case !strings.HasPrefix(line, " "):
			// A package header, "name:" or "name:arch:".
			if h := strings.TrimSuffix(line, ":"); h != line {
				name = h
				inTable = false
			}
		case strings.TrimSpace(line) == "Version table:":
			inTable = true
		case !inTable || name == "":
			continue
		case strings.HasPrefix(line, "        "):
			// An index row: a priority and then the describe text.
			_, describe, ok := strings.Cut(strings.TrimSpace(line), " ")
			if !ok {
				continue
			}
			versions := packages[name]
			if len(versions) == 0 {
				continue
			}
			last := len(versions) - 1
			versions[last].describe = append(versions[last].describe, strings.TrimSpace(describe))
			packages[name] = versions
		case strings.HasPrefix(line, " *** "), strings.HasPrefix(line, "     "):
			fields := strings.Fields(strings.TrimPrefix(strings.TrimSpace(line), "*** "))
			if len(fields) < 1 {
				continue
			}
			packages[name] = append(packages[name], availableVersion{version: fields[0]})
		}
	}
	return packages
}

// isSecurityUpgrade reports whether a waiting update is a security update.
//
// The rule is the distribution's own, read from
// /usr/lib/update-notifier/apt_check.py on a running node: a package counts
// when ANY version newer than the installed one sits in a security pocket. The
// candidate's own pockets are not enough, because a later non-security upload
// to the updates pocket can supersede the security version and mask it, and the
// update being installed still carries that security fix.
func isSecurityUpgrade(versions []availableVersion, installedVersion string, indexes map[string]packageIndex, codename string) bool {
	pockets := securityPockets(codename)
	for _, v := range versions {
		if installedVersion != "" && compareDebianVersions(v.version, installedVersion) <= 0 {
			continue
		}
		for _, describe := range v.describe {
			idx, ok := indexes[describe]
			if !ok {
				continue
			}
			if pockets[packageIndex{origin: idx.origin, archive: idx.archive}] {
				return true
			}
		}
	}
	return false
}

// securityPockets returns the origin and archive pairs that make an index a
// security pocket, for one distribution codename. The set is the one
// /usr/lib/update-notifier/apt_check.py uses, so a RunOS count and the login
// banner's count rest on the same definition.
func securityPockets(codename string) map[packageIndex]bool {
	return map[packageIndex]bool{
		{origin: "Ubuntu", archive: codename + "-security"}:             true,
		{origin: "UbuntuESM", archive: codename + "-infra-security"}:    true,
		{origin: "UbuntuESMApps", archive: codename + "-apps-security"}: true,
	}
}

// parseAptConfig reads the `KEY "VALUE";` lines of `apt-config dump`.
func parseAptConfig(out string) map[string]string {
	values := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(line), ";"))
		key, value, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		values[key] = strings.Trim(strings.TrimSpace(value), `"`)
	}
	return values
}

// historyTimeLayout is apt's own history log time format. apt writes both
// Start-Date and End-Date with strftime "%F  %T", which is a date and a time
// separated by TWO spaces, in the node's local time.
const historyTimeLayout = "2006-01-02  15:04:05"

// lastSuccessfulRun returns the end time of the newest package manager run in
// an apt history log that finished without an error.
//
// apt writes one block per run, separated by a blank line, and records a
// failure as an "Error:" line just before "End-Date:". End-Date is written even
// when the run fails, so the end time alone does not mean success and the Error
// line has to be checked.
func lastSuccessfulRun(history string) (time.Time, bool) {
	var newest time.Time
	var found bool
	for _, block := range strings.Split(strings.ReplaceAll(history, "\r\n", "\n"), "\n\n") {
		var end time.Time
		var haveEnd, failed bool
		for _, line := range strings.Split(block, "\n") {
			line = strings.TrimSpace(line)
			switch {
			case strings.HasPrefix(line, "Error:"):
				failed = true
			case strings.HasPrefix(line, "End-Date:"):
				value := strings.TrimSpace(strings.TrimPrefix(line, "End-Date:"))
				t, err := time.ParseInLocation(historyTimeLayout, value, time.Local)
				if err != nil {
					continue
				}
				end, haveEnd = t, true
			}
		}
		if !haveEnd || failed {
			continue
		}
		if !found || end.After(newest) {
			newest, found = end, true
		}
	}
	return newest.UTC(), found
}

// gunzip reads a gzip stream, which is how logrotate leaves the rotated apt
// history log.
func gunzip(b []byte) (string, error) {
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	defer func() { _ = zr.Close() }()
	plain, err := io.ReadAll(zr)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}
