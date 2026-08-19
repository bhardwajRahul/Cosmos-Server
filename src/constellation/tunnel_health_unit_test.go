package constellation

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/azukaar/cosmos-server/src/utils"
)

// backendServer starts an http server closed at the end of the test.
func backendServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func healthyByName(name string) bool {
	return isTunnelBackendHealthy(utils.ProxyRouteConfig{Name: name})
}

func TestUnitProbeTunnelBackend(t *testing.T) {
	srv := backendServer(t)
	addr := srv.Listener.Addr().String()

	if !probeTunnelBackend(addr) {
		t.Fatalf("probeTunnelBackend(%q) = false on a live backend", addr)
	}

	srv.Close()
	if probeTunnelBackend(addr) {
		t.Errorf("probeTunnelBackend(%q) = true after the backend was closed", addr)
	}
}

func TestUnitTunnelProbeAddress(t *testing.T) {
	setupTestEnv(t, nil)

	tests := []struct {
		name      string
		route     utils.ProxyRouteConfig
		want      string
		probeable bool
	}{
		{"explicit port", utils.ProxyRouteConfig{Mode: "PROXY", Target: "http://backend:8080"}, "backend:8080", true},
		{"default http port", utils.ProxyRouteConfig{Mode: "PROXY", Target: "http://backend"}, "backend:80", true},
		{"default https port", utils.ProxyRouteConfig{Mode: "PROXY", Target: "https://backend"}, "backend:443", true},
		{"static", utils.ProxyRouteConfig{Mode: "STATIC", Target: "/var/www"}, "", false},
		{"spa", utils.ProxyRouteConfig{Mode: "SPA", Target: "/var/www"}, "", false},
		{"redirect", utils.ProxyRouteConfig{Mode: "REDIRECT", Target: "https://elsewhere.com"}, "", false},
		{"non http scheme", utils.ProxyRouteConfig{Mode: "PROXY", Target: "unix:///var/run/x.sock"}, "", false},
		{"empty target", utils.ProxyRouteConfig{Mode: "PROXY", Target: ""}, "", false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			addr, probeable := tunnelProbeAddress(test.route)
			if probeable != test.probeable || addr != test.want {
				t.Errorf("tunnelProbeAddress() = (%q, %v), want (%q, %v)", addr, probeable, test.want, test.probeable)
			}
		})
	}
}

func TestUnitSweepTunnelBackendsWithdrawsDeadBackend(t *testing.T) {
	live := backendServer(t)
	dead := backendServer(t)
	deadTarget := dead.URL
	dead.Close()

	setupTestEnv(t, func(cfg *utils.Config) {
		cfg.HTTPConfig.ProxyConfig.Routes = []utils.ProxyRouteConfig{
			{Name: "live", Mode: "PROXY", Target: live.URL, Tunnel: "_ANY_"},
			{Name: "dead", Mode: "PROXY", Target: deadTarget, Tunnel: "_ANY_"},
			{Name: "static", Mode: "STATIC", Target: "/var/www", Tunnel: "_ANY_"},
			{Name: "not-tunneled", Mode: "PROXY", Target: deadTarget},
		}
	})

	// before any sweep everything is advertised
	if !healthyByName("dead") {
		t.Error("dead route is unhealthy before the first sweep, want fail-open")
	}

	sweepTunnelBackends()
	if !healthyByName("dead") {
		t.Error("dead route withdrawn after a single failed sweep, want the debounce to hold it")
	}

	sweepTunnelBackends()
	if healthyByName("dead") {
		t.Error("dead route still healthy after 2 failed sweeps")
	}
	if !healthyByName("live") {
		t.Error("live route marked unhealthy")
	}
	if !healthyByName("static") {
		t.Error("static route marked unhealthy, it has no probeable backend")
	}

	tunnelHealthMux.RLock()
	_, tracked := tunnelHealth["not-tunneled"]
	_, staticTracked := tunnelHealth["static"]
	tunnelHealthMux.RUnlock()
	if tracked {
		t.Error("non-tunneled route was probed")
	}
	if staticTracked {
		t.Error("static route was probed")
	}
}

func TestUnitSweepTunnelBackendsRecovers(t *testing.T) {
	srv := backendServer(t)
	target := srv.URL
	addr := srv.Listener.Addr().String()
	srv.Close()

	setupTestEnv(t, func(cfg *utils.Config) {
		cfg.HTTPConfig.ProxyConfig.Routes = []utils.ProxyRouteConfig{
			{Name: "flappy", Mode: "PROXY", Target: target, Tunnel: "_ANY_"},
		}
	})

	sweepTunnelBackends()
	sweepTunnelBackends()
	if healthyByName("flappy") {
		t.Fatal("route still healthy after 2 failed sweeps")
	}

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		t.Skip("could not rebind the backend port:", err)
	}
	defer listener.Close()

	sweepTunnelBackends()
	if !healthyByName("flappy") {
		t.Error("route not restored on the first successful sweep")
	}
}

func TestUnitGetAllTunneledRoutesSkipsUnhealthyBackends(t *testing.T) {
	live := backendServer(t)
	dead := backendServer(t)
	deadTarget := dead.URL
	dead.Close()

	setupTestEnv(t, func(cfg *utils.Config) {
		cfg.ConstellationConfig.ThisDeviceName = "node-a"
		cfg.HTTPConfig.ProxyConfig.Routes = []utils.ProxyRouteConfig{
			{Name: "live", Mode: "PROXY", Target: live.URL, Tunnel: "_ANY_"},
			{Name: "dead", Mode: "PROXY", Target: deadTarget, Tunnel: "_ANY_"},
		}
	})
	writeNebulaYML(t, map[string]interface{}{
		"cstln_device_name": "node-a",
		"cstln_ip":          "192.168.201.1",
		"cstln_api_key":     "test-key",
	})

	names := func() []string {
		out := []string{}
		for _, route := range GetAllTunneledRoutes() {
			out = append(out, route.Name)
		}
		return out
	}

	if got := names(); len(got) != 2 {
		t.Fatalf("before the first sweep GetAllTunneledRoutes() advertised %v, want both routes", got)
	}

	sweepTunnelBackends()
	sweepTunnelBackends()

	got := names()
	if len(got) != 1 || got[0] != "live" {
		t.Errorf("GetAllTunneledRoutes() = %v, want [live] once the dead backend is withdrawn", got)
	}
}
