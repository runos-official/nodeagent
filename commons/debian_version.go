package commons

import (
	"fmt"
	"strconv"
	"strings"
)

// Debian package version comparison, as dpkg does it.
//
// RunOS holds packages at a chosen version, and a controlled maintenance run
// may move a held package ONLY to a newer distribution revision of the upstream
// version the node already runs. Deciding that needs a real version comparison:
// a prefix test calls 1.7.2 and 1.7.24 the same version, and a string compare
// calls revision 10 older than revision 9.
//
// The rules below come from Debian Policy 5.6.12 and from dpkg's own
// lib/dpkg/version.c (order, verrevcmp, dpkg_version_compare). The test file
// cross-checks this parser against `dpkg --compare-versions` on this host.

// debianVersion is one parsed version: [epoch:]upstream[-revision].
type debianVersion struct {
	epoch    int
	upstream string
	revision string
}

// parseDebianVersion reads a Debian version string. It returns an error for
// anything it cannot read, and never guesses, because each caller picks its own
// safe direction on an error and a guess would take that choice away.
func parseDebianVersion(v string) (debianVersion, error) {
	var out debianVersion
	if strings.TrimSpace(v) != v {
		return out, fmt.Errorf("debian version %q has surrounding whitespace", v)
	}
	if v == "" {
		return out, fmt.Errorf("debian version is empty")
	}

	// The epoch is the part before the FIRST colon, and it must be an unsigned
	// integer. A colon with no epoch, or a non-numeric epoch, is an error.
	rest := v
	if i := strings.IndexByte(rest, ':'); i >= 0 {
		epoch := rest[:i]
		if epoch == "" {
			return out, fmt.Errorf("debian version %q has an empty epoch", v)
		}
		n, err := strconv.ParseUint(epoch, 10, 32)
		if err != nil {
			return out, fmt.Errorf("debian version %q has a non-numeric epoch %q", v, epoch)
		}
		out.epoch = int(n)
		rest = rest[i+1:]
	}

	// The revision is the part after the LAST hyphen. A hyphen promises a
	// revision, so an empty one is an error.
	if i := strings.LastIndexByte(rest, '-'); i >= 0 {
		out.upstream = rest[:i]
		out.revision = rest[i+1:]
		if out.revision == "" {
			return out, fmt.Errorf("debian version %q ends in a hyphen with no revision", v)
		}
	} else {
		out.upstream = rest
	}

	if out.upstream == "" {
		return out, fmt.Errorf("debian version %q has an empty upstream version", v)
	}
	// Debian Policy 5.6.12: the upstream version must start with a digit. dpkg
	// refuses anything else, and so does this parser: "dev" or "unknown" from a
	// failed read must never compare as a version.
	if !isASCIIDigit(out.upstream[0]) {
		return out, fmt.Errorf("debian version %q does not start with a digit", v)
	}
	if err := checkVersionChars(v, "upstream version", out.upstream, "+-.~:"); err != nil {
		return out, err
	}
	if err := checkVersionChars(v, "revision", out.revision, "+.~"); err != nil {
		return out, err
	}
	return out, nil
}

// checkVersionChars refuses any character Debian Policy 5.6.12 does not allow
// in that part of a version. An unexpected character means the string is not a
// version, and guessing at it is how a parser silently moves the wrong package.
func checkVersionChars(whole, part, s, extra string) error {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if isASCIIDigit(c) || isASCIILetter(c) || strings.IndexByte(extra, c) >= 0 {
			continue
		}
		return fmt.Errorf("debian version %q has an invalid character %q in its %s", whole, string(c), part)
	}
	return nil
}

func isASCIIDigit(c byte) bool  { return c >= '0' && c <= '9' }
func isASCIILetter(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

// order is dpkg's order(): the weight one character carries when a non-digit run
// is compared. A digit and the end of the string both weigh 0, a letter weighs
// its own code, a tilde weighs less than everything, and any other character
// weighs more than every letter.
func order(c byte) int {
	switch {
	case isASCIIDigit(c):
		return 0
	case isASCIILetter(c):
		return int(c)
	case c == '~':
		return -1
	case c == 0:
		return 0
	default:
		return int(c) + 256
	}
}

// at returns the byte at i, or 0 past the end, so this parser can walk a string
// the way dpkg's C code walks a NUL-terminated one.
func at(s string, i int) byte {
	if i >= len(s) {
		return 0
	}
	return s[i]
}

// verrevcmp is dpkg's verrevcmp(): it compares one version part by alternating
// a non-digit run, compared through order, with a digit run, compared as a
// number. Leading zeros are skipped, so revision 01 equals revision 1.
func verrevcmp(a, b string) int {
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		firstDiff := 0

		for (at(a, i) != 0 && !isASCIIDigit(at(a, i))) || (at(b, j) != 0 && !isASCIIDigit(at(b, j))) {
			ac, bc := order(at(a, i)), order(at(b, j))
			if ac != bc {
				return ac - bc
			}
			i++
			j++
		}
		for at(a, i) == '0' {
			i++
		}
		for at(b, j) == '0' {
			j++
		}
		for isASCIIDigit(at(a, i)) && isASCIIDigit(at(b, j)) {
			if firstDiff == 0 {
				firstDiff = int(at(a, i)) - int(at(b, j))
			}
			i++
			j++
		}
		// A digit left on one side means that side's number has more digits, so
		// that side is the larger number.
		if isASCIIDigit(at(a, i)) {
			return 1
		}
		if isASCIIDigit(at(b, j)) {
			return -1
		}
		if firstDiff != 0 {
			return firstDiff
		}
	}
	return 0
}

// compareDebianVersions returns a negative number when a sorts below b, zero
// when they are equal, and a positive number when a sorts above b. It is
// dpkg_version_compare(): the epoch first as an integer, then the upstream
// version, then the revision.
func compareDebianVersions(a, b string) (int, error) {
	pa, err := parseDebianVersion(a)
	if err != nil {
		return 0, err
	}
	pb, err := parseDebianVersion(b)
	if err != nil {
		return 0, err
	}
	if pa.epoch != pb.epoch {
		return pa.epoch - pb.epoch, nil
	}
	if r := verrevcmp(pa.upstream, pb.upstream); r != 0 {
		return r, nil
	}
	return verrevcmp(pa.revision, pb.revision), nil
}

// SameUpstreamVersion reports whether two Debian versions carry the same epoch
// and the same upstream version, whatever their revisions. The explicit
// Kubernetes node upgrade asks this: it installs an upstream version from the
// version matrix and leaves the revision to a controlled maintenance run.
//
// It returns an error when either version cannot be read. The caller decides
// what to do with that error, because the safe direction differs by caller.
func SameUpstreamVersion(a, b string) (bool, error) {
	pa, err := parseDebianVersion(a)
	if err != nil {
		return false, err
	}
	pb, err := parseDebianVersion(b)
	if err != nil {
		return false, err
	}
	if pa.epoch != pb.epoch {
		return false, nil
	}
	return verrevcmp(pa.upstream, pb.upstream) == 0, nil
}

// IsSameUpstreamNewerRevision reports whether candidate is a newer distribution
// revision of the same upstream version as installed. It is the decision a
// controlled maintenance run makes for one held package: a held package may
// move to a security revision of the version the node already runs, and never
// across an upstream version and never downwards.
//
// It returns an error when either version cannot be read. A controlled run must
// NOT move a package whose version it cannot read, so that caller treats an
// error as "do not move"; the returned bool is already false.
func IsSameUpstreamNewerRevision(installed, candidate string) (bool, error) {
	same, err := SameUpstreamVersion(installed, candidate)
	if err != nil || !same {
		return false, err
	}
	pi, err := parseDebianVersion(installed)
	if err != nil {
		return false, err
	}
	pc, err := parseDebianVersion(candidate)
	if err != nil {
		return false, err
	}
	return verrevcmp(pc.revision, pi.revision) > 0, nil
}
