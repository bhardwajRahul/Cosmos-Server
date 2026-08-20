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
	for _, mode := range []string{"least_conn", "load-based"} {
		if utils.IsValidLBMode(mode) {
			t.Errorf("IsValidLBMode(%q) = true, want false", mode)
		}
		lb := &TunnelLoadBalancer{}
		if got := lb.Select(keys, "route", mode, false, ""); got != "a" {
			t.Errorf("Select(mode=%q) = %q, want a", mode, got)
		}
	}
}

func TestSelectTargetLoadBased(t *testing.T) {
	if !utils.IsValidLBMode("load_based") {
		t.Fatal("IsValidLBMode(load_based) = false, want true")
	}

	targets := []utils.TunnelTarget{
		{DeviceName: "a", CPUPercent: 20, RAMPercent: 90, MonitoringOn: true},
		{DeviceName: "b", CPUPercent: 50, RAMPercent: 40, MonitoringOn: true},
		{DeviceName: "c", CPUPercent: 80, RAMPercent: 10, MonitoringOn: true},
	}

	// b has the lowest max(cpu, ram) = 50 (a=90, c=80); repeat calls stay on b
	lb := &TunnelLoadBalancer{}
	for i := 0; i < 3; i++ {
		got := lb.SelectTarget(targets, "route", "load_based", false, "")
		if got == nil || got.DeviceName != "b" {
			t.Fatalf("SelectTarget(load_based) call %d = %v, want b", i, got)
		}
	}

	// LOAD_BASED matches case-insensitively like the other modes
	if got := lb.SelectTarget(targets, "route", "LOAD_BASED", false, ""); got == nil || got.DeviceName != "b" {
		t.Errorf("SelectTarget(LOAD_BASED) = %v, want b", got)
	}
}

func TestSelectTargetLoadBasedFallsBackToRoundRobin(t *testing.T) {
	// one unmonitored candidate poisons the metrics: all-or-nothing fallback
	targets := []utils.TunnelTarget{
		{DeviceName: "a", CPUPercent: 20, RAMPercent: 20, MonitoringOn: true},
		{DeviceName: "b", MonitoringOn: false},
	}

	lb := &TunnelLoadBalancer{}
	for i, want := range []string{"a", "b", "a", "b"} {
		got := lb.SelectTarget(targets, "route", "load_based", false, "")
		if got == nil || got.DeviceName != want {
			t.Errorf("SelectTarget call %d = %v, want %s", i, got, want)
		}
	}

	// the keys-only path has no metrics at all: same fallback
	lb2 := &TunnelLoadBalancer{}
	for i, want := range []string{"x", "y", "x"} {
		if got := lb2.Select([]string{"x", "y"}, "route", "load_based", false, ""); got != want {
			t.Errorf("Select call %d = %q, want %q", i, got, want)
		}
	}
}
