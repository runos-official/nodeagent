package vip

import (
	"net"
	"testing"
)

func mustCIDR(t *testing.T, s string) *net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatalf("ParseCIDR(%q): %v", s, err)
	}
	return n
}

func addrs(nets ...*net.IPNet) []net.Addr {
	out := make([]net.Addr, 0, len(nets))
	for _, n := range nets {
		out = append(out, n)
	}
	return out
}

func TestParseClusterIdx(t *testing.T) {
	cases := []struct {
		name    string
		addrs   []net.Addr
		want    int
		wantErr bool
	}{
		{
			name:  "plain node address",
			addrs: addrs(mustCIDR(t, "172.24.7.10/24")),
			want:  7,
		},
		{
			name: "ignores VIP address, picks node address",
			addrs: addrs(
				mustCIDR(t, "172.24.7.254/32"),
				mustCIDR(t, "172.24.7.10/24"),
			),
			want: 7,
		},
		{
			name: "ignores non-172.24 addresses",
			addrs: addrs(
				mustCIDR(t, "10.0.0.1/24"),
				mustCIDR(t, "172.24.3.15/24"),
			),
			want: 3,
		},
		{
			name:    "no 172.24 address",
			addrs:   addrs(mustCIDR(t, "10.0.0.1/24")),
			wantErr: true,
		},
		{
			name:    "only VIP address, no node address",
			addrs:   addrs(mustCIDR(t, "172.24.7.254/32")),
			wantErr: true,
		},
		{
			name:    "empty address list",
			addrs:   nil,
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseClusterIdx(tc.addrs)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got idx=%d", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got idx=%d, want %d", got, tc.want)
			}
		})
	}
}

func TestCidr(t *testing.T) {
	cases := []struct {
		idx  int
		want string
	}{
		{1, "172.24.1.254/32"},
		{7, "172.24.7.254/32"},
		{255, "172.24.255.254/32"},
	}
	for _, tc := range cases {
		if got := Cidr(tc.idx); got != tc.want {
			t.Errorf("Cidr(%d) = %q, want %q", tc.idx, got, tc.want)
		}
	}
}
