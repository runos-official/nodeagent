package agentstream

import "testing"

// The explicit Kubernetes node upgrade decides here whether to install the
// target containerd version. The installed value is the full Debian version
// dpkg reports; the target is an upstream version from the version matrix.
//
// A prefix test is wrong in two directions, and this table pins both:
// an installed version already at the target upstream version must NOT be
// reinstalled by this path, and an installed 1.7.24 with a target of 1.7.2 must
// install, because 1.7.2 is a DIFFERENT upstream version that merely spells the
// start of 1.7.24.
func TestContainerdNeedsInstall(t *testing.T) {
	cases := []struct {
		name      string
		installed string
		target    string
		want      bool
		why       string
	}{
		{
			name:      "same upstream version, nothing to do",
			installed: "1.7.24-0ubuntu1~24.04.1", target: "1.7.24", want: false,
			why: "the node already runs the target upstream version",
		},
		{
			name:      "older upstream version installs",
			installed: "1.7.23-0ubuntu1~24.04.1", target: "1.7.24", want: true,
			why: "the node runs an older upstream version",
		},
		{
			name:      "newer upstream version installs, because the matrix governs",
			installed: "2.2.1-0ubuntu1~24.04.4", target: "1.7.24", want: true,
			why: "an explicit upgrade installs the version the matrix names, up or down",
		},
		{
			name:      "target is a shorter different version, not a prefix match",
			installed: "1.7.24-0ubuntu1~24.04.1", target: "1.7.2", want: true,
			why: "1.7.2 and 1.7.24 are different upstream versions; the prefix test skipped this",
		},
		{
			name:      "same upstream version at a newer revision is left alone",
			installed: "1.7.24-0ubuntu1~24.04.1", target: "1.7.24", want: false,
			why: "the revision move belongs to the controlled maintenance run, not to this path",
		},
		{
			name:      "an unreadable installed version installs the target",
			installed: "", target: "1.7.24", want: true,
			why: "an explicit upgrade must still install what the user asked for",
		},
		{
			name:      "an unparseable installed version installs the target",
			installed: "not-a-version", target: "1.7.24", want: true,
			why: "an explicit upgrade must still install what the user asked for",
		},
		{
			name:      "an epoch on the installed version does not hide the same upstream version",
			installed: "1:1.7.24-0ubuntu1", target: "1:1.7.24", want: false,
			why: "the epoch matches and so does the upstream version",
		},
		{
			name:      "a differing epoch is a different version",
			installed: "1:1.7.24-0ubuntu1", target: "1.7.24", want: true,
			why: "epoch 1 and epoch 0 are different versions, and the prefix test never saw this",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := containerdNeedsInstall(c.installed, c.target); got != c.want {
				t.Errorf("containerdNeedsInstall(%q, %q) = %v, want %v: %s", c.installed, c.target, got, c.want, c.why)
			}
		})
	}
}
