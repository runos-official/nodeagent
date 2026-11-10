package commons

import (
	"os/exec"
	"strings"
	"testing"
)

// IsSameUpstreamNewerRevision is the decision a controlled maintenance run
// makes for one held package: may this package move? It may move only to a
// newer DISTRIBUTION REVISION of the upstream version the node already runs.
// A newer upstream version is a different package version and a maintenance run
// never takes it.
func TestIsSameUpstreamNewerRevision(t *testing.T) {
	cases := []struct {
		name      string
		installed string
		candidate string
		want      bool
		wantErr   bool
		why       string
	}{
		// The five cases acceptance criterion 3 names.
		{
			name:      "same upstream version, newer revision, moves",
			installed: "2.8.5-1ubuntu3", candidate: "2.8.5-1ubuntu4", want: true,
			why: "a security revision of the version the node already runs",
		},
		{
			name:      "same upstream version, same revision, does not move",
			installed: "2.8.5-1ubuntu3", candidate: "2.8.5-1ubuntu3", want: false,
			why: "nothing to install",
		},
		{
			name:      "newer upstream version does not move",
			installed: "2.8.5-1ubuntu3", candidate: "2.9.0-1ubuntu1", want: false,
			why: "a maintenance run never crosses an upstream version",
		},
		{
			name:      "older revision does not move",
			installed: "2.8.5-1ubuntu4", candidate: "2.8.5-1ubuntu3", want: false,
			why: "a maintenance run never downgrades",
		},
		{
			name:      "an unreadable candidate does not move and reports an error",
			installed: "2.8.5-1ubuntu3", candidate: "not-a-version", want: false, wantErr: true,
			why: "the caller must not move a package whose version it cannot read",
		},
		// The cases the plan adds, because a version comparison written from
		// memory is wrong on exactly these.
		{
			name:      "an unreadable installed version does not move and reports an error",
			installed: "", candidate: "2.8.5-1ubuntu3", want: false, wantErr: true,
			why: "an empty dpkg read must never be treated as an older version",
		},
		{
			name:      "a different epoch is not the same upstream version",
			installed: "1:2.8.5-1ubuntu3", candidate: "2:2.8.5-1ubuntu4", want: false,
			why: "the epoch is part of the version identity",
		},
		{
			name:      "an absent epoch and epoch 1 are different versions",
			installed: "2.8.5-1ubuntu3", candidate: "1:2.8.5-1ubuntu4", want: false,
			why: "an absent epoch compares as 0, not as any epoch",
		},
		{
			name:      "a matching epoch does not block the move",
			installed: "1:2.8.5-1ubuntu3", candidate: "1:2.8.5-1ubuntu4", want: true,
			why: "same epoch, same upstream version, newer revision",
		},
		{
			name:      "a tilde revision orders below its plain form, so it moves up to it",
			installed: "2.8.5-1~bpo1", candidate: "2.8.5-1", want: true,
			why: "a tilde sorts below everything, so 1~bpo1 is older than 1",
		},
		{
			name:      "the plain form does not move down to its tilde form",
			installed: "2.8.5-1", candidate: "2.8.5-1~bpo1", want: false,
			why: "that would be a downgrade",
		},
		{
			name:      "leading zeros do not change a revision",
			installed: "2.8.5-01", candidate: "2.8.5-1", want: false,
			why: "revision 01 and revision 1 are the same number",
		},
		{
			name:      "a numerically newer revision moves even with leading zeros",
			installed: "2.8.5-1", candidate: "2.8.5-02", want: true,
			why: "revision 02 is the number 2, which is newer than 1",
		},
		{
			name:      "an absent revision compares as zero and is older than revision 1",
			installed: "2.8.5", candidate: "2.8.5-1", want: true,
			why: "an absent revision compares as 0",
		},
		{
			name:      "an absent revision and revision 0 are the same",
			installed: "2.8.5-0", candidate: "2.8.5", want: false,
			why: "an absent revision compares as 0",
		},
		{
			name:      "a hyphen inside the upstream version does not confuse the split",
			installed: "0.6-42-ga14256a-0.2build1", candidate: "0.6-42-ga14256a-0.2build2", want: true,
			why: "the revision is the part after the LAST hyphen, so the upstream version matches",
		},
		{
			name:      "a hyphenated upstream version that really differs does not move",
			installed: "0.6-42-ga14256a-0.2build1", candidate: "0.6-43-gb14256a-0.2build1", want: false,
			why: "the upstream version differs before the last hyphen",
		},
		{
			name:      "a real kernel-style revision moves",
			installed: "1.0.20210914-1ubuntu4", candidate: "1.0.20210914-1ubuntu4.1", want: true,
			why: "4.1 is a newer revision than 4",
		},
		{
			name:      "an empty candidate does not move and reports an error",
			installed: "2.8.5-1ubuntu3", candidate: "", want: false, wantErr: true,
			why: "an empty apt read must never be treated as a newer version",
		},
		{
			name:      "an upstream version that does not start with a digit is an error",
			installed: "abc-1", candidate: "2.8.5-1", want: false, wantErr: true,
			why: "Debian policy requires an upstream version to start with a digit",
		},
		{
			name:      "a trailing hyphen with no revision is an error",
			installed: "2.8.5-", candidate: "2.8.5-1", want: false, wantErr: true,
			why: "a hyphen promises a revision",
		},
		{
			name:      "a non-numeric epoch is an error",
			installed: "x:2.8.5-1", candidate: "2.8.5-1", want: false, wantErr: true,
			why: "Debian policy requires the epoch to be an unsigned integer",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := IsSameUpstreamNewerRevision(c.installed, c.candidate)
			if c.wantErr && err == nil {
				t.Errorf("IsSameUpstreamNewerRevision(%q, %q) returned no error, want one: %s", c.installed, c.candidate, c.why)
			}
			if !c.wantErr && err != nil {
				t.Errorf("IsSameUpstreamNewerRevision(%q, %q) returned error %v, want none: %s", c.installed, c.candidate, err, c.why)
			}
			if got != c.want {
				t.Errorf("IsSameUpstreamNewerRevision(%q, %q) = %v, want %v: %s", c.installed, c.candidate, got, c.want, c.why)
			}
		})
	}
}

// SameUpstreamVersion answers the other question: is the node already on this
// upstream version, whatever revision it carries? The explicit Kubernetes
// upgrade asks it, because it installs an upstream version and leaves the
// revision to the controlled maintenance run.
func TestSameUpstreamVersion(t *testing.T) {
	cases := []struct {
		a, b    string
		want    bool
		wantErr bool
		why     string
	}{
		{a: "1.7.24-0ubuntu1~24.04.1", b: "1.7.24", want: true, why: "the revision does not count"},
		{a: "1.7.24-0ubuntu1~24.04.1", b: "1.7.2", want: false, why: "1.7.2 is a different upstream version that spells the start of 1.7.24"},
		{a: "1.7.24", b: "1.7.24", want: true, why: "identical"},
		{a: "1:1.7.24-1", b: "1:1.7.24", want: true, why: "same epoch, same upstream version"},
		{a: "1:1.7.24-1", b: "1.7.24", want: false, why: "epoch 1 is not epoch 0"},
		{a: "2.2.1-0ubuntu1~24.04.4", b: "1.7.24", want: false, why: "different upstream version"},
		{a: "1.7.24-1", b: "01.7.24", want: true, why: "leading zeros do not change a number"},
		{a: "", b: "1.7.24", want: false, wantErr: true, why: "an empty dpkg read is not a version"},
		{a: "1.7.24-1", b: "", want: false, wantErr: true, why: "an empty target is not a version"},
		{a: "not-a-version", b: "1.7.24", want: false, wantErr: true, why: "the upstream version must start with a digit"},
	}
	for _, c := range cases {
		got, err := SameUpstreamVersion(c.a, c.b)
		if c.wantErr != (err != nil) {
			t.Errorf("SameUpstreamVersion(%q, %q) error = %v, wantErr %v: %s", c.a, c.b, err, c.wantErr, c.why)
		}
		if got != c.want {
			t.Errorf("SameUpstreamVersion(%q, %q) = %v, want %v: %s", c.a, c.b, got, c.want, c.why)
		}
	}
}

// The only defence against a parser that reads Debian policy wrongly: dpkg
// itself. This is not a substitute for the table tests above, which pin the
// cases this product depends on; it is the check that the ORDER the parser
// produces is the order dpkg produces.
func TestCompareDebianVersionsAgreesWithDpkg(t *testing.T) {
	dpkg, err := exec.LookPath("dpkg")
	if err != nil {
		t.Skip("dpkg is absent on this host, so the cross-check cannot run")
	}
	pairs := [][2]string{
		{"2.8.5-1ubuntu3", "2.8.5-1ubuntu4"},
		{"2.8.5-1ubuntu3", "2.8.5-1ubuntu3"},
		{"2.8.5-1ubuntu4", "2.8.5-1ubuntu3"},
		{"2.8.5", "2.8.5-1"},
		{"2.8.5-0", "2.8.5"},
		{"2.8.5-01", "2.8.5-1"},
		{"2.8.5-1", "2.8.5-02"},
		{"2.8.5-1~bpo1", "2.8.5-1"},
		{"1:2.8.5-1", "2:2.8.5-1"},
		{"2.8.5-1", "1:2.8.5-1"},
		{"1.7.2-1", "1.7.24-1"},
		{"1.7.24-1", "1.7.3-1"},
		{"1.7.24-1", "01.7.24-1"},
		{"2.2.1-0ubuntu1~24.04.4", "2.2.1-0ubuntu1~24.04.10"},
		{"1.0.20210914-1ubuntu4", "1.0.20210914-1ubuntu4.1"},
		{"0.6-42-ga14256a-0.2build1", "0.6-42-ga14256a-0.2build2"},
		{"1:9.18.39-0ubuntu0.24.04.9", "1:9.18.39-0ubuntu0.24.04.10"},
		{"1.36.4-1.1", "1.36.5-1.1"},
		{"1.7.24-1", "1.7.24-1~exp1"},
		{"3.4.30-1ubuntu0.24.04.3", "3.4.30-1ubuntu0.24.04.3"},
	}
	for _, p := range pairs {
		got, err := compareDebianVersions(p[0], p[1])
		if err != nil {
			t.Errorf("compareDebianVersions(%q, %q) returned error %v, but dpkg can read both", p[0], p[1], err)
			continue
		}
		want := dpkgCompare(t, dpkg, p[0], p[1])
		if sign(got) != want {
			t.Errorf("compareDebianVersions(%q, %q) sign = %d, dpkg says %d", p[0], p[1], sign(got), want)
		}
	}
}

// dpkgCompare returns -1, 0 or 1 as dpkg orders a against b.
func dpkgCompare(t *testing.T, dpkg, a, b string) int {
	t.Helper()
	for _, c := range []struct {
		op   string
		want int
	}{{"lt", -1}, {"eq", 0}, {"gt", 1}} {
		cmd := exec.Command(dpkg, "--compare-versions", a, c.op, b)
		if err := cmd.Run(); err == nil {
			return c.want
		} else if _, ok := err.(*exec.ExitError); !ok {
			t.Fatalf("running dpkg --compare-versions %q %s %q: %v", a, c.op, b, err)
		}
	}
	t.Fatalf("dpkg says %q is neither lt, eq nor gt %q", a, b)
	return 0
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

// A guard on the test data itself: every version this file compares must be one
// dpkg accepts, or a disagreement would mean the test data is wrong and not the
// parser.
func TestCrossCheckVersionsAreValidForDpkg(t *testing.T) {
	dpkg, err := exec.LookPath("dpkg")
	if err != nil {
		t.Skip("dpkg is absent on this host")
	}
	for _, v := range []string{"2.8.5-1ubuntu3", "2.8.5", "2.8.5-01", "1:2.8.5-1", "0.6-42-ga14256a-0.2build1"} {
		out, err := exec.Command(dpkg, "--compare-versions", v, "eq", v).CombinedOutput()
		if err != nil {
			t.Errorf("dpkg rejects the test version %q: %v (%s)", v, err, strings.TrimSpace(string(out)))
		}
	}
}
