package proxy

import (
	"testing"

	"github.com/azukaar/cosmos-server/src/utils"
)

// Select must implement exactly the modes utils.LBModes allows, and must not silently
// treat an unknown mode as a valid policy.
func TestSelectModes(t *testing.T) {
	keys := []string{"a", "b", "c"}

	for _, mode := range []string{"", "first", "FIRST"} {
		lb := &TunnelLoadBalancer{}
		for i := 0; i < 3; i++ {
			if got := lb.Select(keys, "route", mode, false, ""); got != "a" {
				t.Errorf("Select(mode=%q) call %d = %q, want a", mode, i, got)
			}
		}
	}

	for _, mode := range []string{"round_robin", "ROUND_ROBIN"} {
		lb := &TunnelLoadBalancer{}
		for i, want := range keys {
			if got := lb.Select(keys, "route", mode, false, ""); got != want {
				t.Errorf("Select(mode=%q) call %d = %q, want %q", mode, i, got, want)
			}
		}
	}

	// unknown modes still have to serve traffic, but only from the first target
	for _, mode := range []string{"load_based", "least_conn"} {
		if utils.IsValidLBMode(mode) {
			t.Errorf("IsValidLBMode(%q) = true, want false", mode)
		}
		lb := &TunnelLoadBalancer{}
		if got := lb.Select(keys, "route", mode, false, ""); got != "a" {
			t.Errorf("Select(mode=%q) = %q, want a", mode, got)
		}
	}
}
