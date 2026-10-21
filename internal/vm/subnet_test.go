package vm

import (
	"net/netip"
	"testing"
)

func TestFreeGuestSubnet(t *testing.T) {
	pp := func(ss ...string) []netip.Prefix {
		var out []netip.Prefix
		for _, s := range ss {
			out = append(out, netip.MustParsePrefix(s))
		}
		return out
	}
	for _, tc := range []struct {
		used []netip.Prefix
		want string
	}{
		{nil, "192.168.64.0/24"},
		{pp("10.0.0.0/8", "127.0.0.0/8"), "192.168.64.0/24"},
		// a GitHub macOS runner: the host itself is on vmnet's default
		{pp("192.168.64.0/24"), "192.168.65.0/24"},
		{pp("192.168.64.0/24", "192.168.65.0/24"), "192.168.66.0/24"},
		// a wider host network covers several candidates
		{pp("192.168.64.0/22"), "192.168.68.0/24"},
	} {
		got, err := freeGuestSubnet(tc.used)
		if err != nil || got.String() != tc.want {
			t.Errorf("freeGuestSubnet(%v) = %v, %v; want %s", tc.used, got, err, tc.want)
		}
	}
	if _, err := freeGuestSubnet(pp("192.168.0.0/16")); err == nil {
		t.Error("want an error when 192.168.0.0/16 is taken")
	}
}

func TestGuestNetSpec(t *testing.T) {
	if got, want := guestNetSpec(netip.MustParsePrefix("192.168.65.0/24")), "192.168.65.2/24,192.168.65.1,192.168.65.1"; got != want {
		t.Errorf("guestNetSpec = %q, want %q", got, want)
	}
}
