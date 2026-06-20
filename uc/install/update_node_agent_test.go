package install

import "testing"

func TestBuildUpdateURL(t *testing.T) {
	const installer = "https://get.runos.com"

	cases := []struct {
		name      string
		installer string
		version   string
		want      string
	}{
		{
			// Empty version is fail-closed: no query, so the installer never
			// falls back to a floating "latest".
			name:      "empty version, no query",
			installer: installer,
			version:   "",
			want:      "https://get.runos.com/update",
		},
		{
			// Leading "v" is stripped.
			name:      "v-prefixed version stripped",
			installer: installer,
			version:   "v0.24.0",
			want:      "https://get.runos.com/update?version=0.24.0",
		},
		{
			name:      "bare version passes through",
			installer: installer,
			version:   "0.24.0",
			want:      "https://get.runos.com/update?version=0.24.0",
		},
		{
			// A character that needs query-escaping must be escaped (a space
			// becomes %20), so the version can never break the URL.
			name:      "version needing escaping is escaped",
			installer: installer,
			version:   "0.24.0 beta",
			want:      "https://get.runos.com/update?version=0.24.0+beta",
		},
		{
			name:      "ampersand in version is escaped",
			installer: installer,
			version:   "0.24.0&x=1",
			want:      "https://get.runos.com/update?version=0.24.0%26x%3D1",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildUpdateURL(tc.installer, tc.version)
			if got != tc.want {
				t.Fatalf("buildUpdateURL(%q, %q) = %q, want %q", tc.installer, tc.version, got, tc.want)
			}
		})
	}
}
