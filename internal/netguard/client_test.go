package netguard

import (
	"net/netip"
	"testing"
)

func TestPublicAddresses(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "::1", "::ffff:127.0.0.1", "10.1.2.3", "169.254.169.254", "100.64.0.1", "2001:db8::1", "64:ff9b::a00:1"} {
		if Public(netip.MustParseAddr(s)) {
			t.Errorf("allowed non-public address %s", s)
		}
	}
	for _, s := range []string{"1.1.1.1", "2606:4700:4700::1111"} {
		if !Public(netip.MustParseAddr(s)) {
			t.Errorf("rejected public address %s", s)
		}
	}
}
