package fcnet

import "testing"

// TestLastOctet pins the IP-tail extraction that names taps and MACs.
func TestLastOctet(t *testing.T) {
	cases := map[string]string{
		"10.89.0.200": "200",
		"10.89.0.5":   "5",
		"192.168.1.1": "1",
		"nodot":       "nodot", // no '.', returned as-is
		"":            "",
	}
	for in, want := range cases {
		if got := lastOctet(in); got != want {
			t.Errorf("lastOctet(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestTapName pins the "fdt"+octet tap naming.
func TestTapName(t *testing.T) {
	if got := TapName("10.89.0.200"); got != "fdt200" {
		t.Errorf("TapName = %q, want fdt200", got)
	}
	if got := TapName("10.89.0.5"); got != "fdt5" {
		t.Errorf("TapName = %q, want fdt5", got)
	}
}

// TestGuestMAC pins the stable per-IP MAC derivation (octet -> 2-hex low byte).
func TestGuestMAC(t *testing.T) {
	cases := map[string]string{
		"10.89.0.200": "02:fc:0a:59:00:c8", // 200 = 0xc8
		"10.89.0.5":   "02:fc:0a:59:00:05",
		"10.89.0.16":  "02:fc:0a:59:00:10",
		"bad":         "02:fc:0a:59:00:00", // non-numeric octet -> Atoi 0
	}
	for in, want := range cases {
		if got := GuestMAC(in); got != want {
			t.Errorf("GuestMAC(%q) = %q, want %q", in, got, want)
		}
	}
}
