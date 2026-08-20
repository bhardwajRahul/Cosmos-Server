package configapi

import (
	"strings"
	"testing"

	"github.com/azukaar/cosmos-server/src/utils"
)

func TestValidateRouteLBMode(t *testing.T) {
	valid := []string{"", "first", "FIRST", "round_robin", "ROUND_ROBIN", "Round_Robin", "load_based", "LOAD_BASED"}
	for _, mode := range valid {
		route := utils.ProxyRouteConfig{Name: "r", LBMode: mode}
		if msg := validateRoute(route); msg != "" {
			t.Errorf("validateRoute(LBMode=%q) = %q, want accepted", mode, msg)
		}
	}

	invalid := []string{"load-based", "least_conn", "weighted", "roundrobin", "round robin", " "}
	for _, mode := range invalid {
		route := utils.ProxyRouteConfig{Name: "r", LBMode: mode}
		msg := validateRoute(route)
		if msg == "" {
			t.Errorf("validateRoute(LBMode=%q) accepted, want rejected", mode)
			continue
		}
		if !strings.Contains(msg, mode) || !strings.Contains(msg, "round_robin") {
			t.Errorf("validateRoute(LBMode=%q) = %q, want the rejected mode and the supported ones named", mode, msg)
		}
	}
}
