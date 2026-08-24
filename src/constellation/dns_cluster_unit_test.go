package constellation

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/azukaar/cosmos-server/src/utils"
)

func seedClusterDNS(t *testing.T, m map[string]clusterHostname) {
	t.Helper()
	setClusterDNS(m)
	t.Cleanup(func() { setClusterDNS(map[string]clusterHostname{}) })
}

func TestUnitLocalHostnames(t *testing.T) {
	setupTestEnv(t, func(cfg *utils.Config) {
		cfg.HTTPConfig.Hostname = "cosmos.local:443"
		cfg.HTTPConfig.ProxyConfig.Routes = []utils.ProxyRouteConfig{
			{Name: "plain", UseHost: true, Host: "plain.local:8443"},
			{Name: "dup", UseHost: true, Host: "plain.local"},
			{Name: "tunneled", UseHost: true, Host: "tunneled.local", Tunnel: "_ANY_"},
			{Name: "aliased", UseHost: true, Host: "private.local", TunneledHost: "public.local:8443", Tunnel: "_ANY_"},
			{Name: "nohost", UseHost: false, Host: "ignored.local"},
			{Name: "multi", UseHost: true, Host: "a.local, b.local"},
		}
	})

	want := []string{"cosmos.local", "plain.local", "private.local"}
	if got := localHostnames(); !reflect.DeepEqual(got, want) {
		t.Errorf("localHostnames() = %v, want %v", got, want)
	}
}

func TestUnitBuildClusterDNS(t *testing.T) {
	heartbeats := []NodeHeartbeat{
		{DeviceName: "b", IP: "192.168.201.2/24", Hostnames: []string{"b.local", "shared.local"},
			Tunnels: []utils.ProxyRouteConfig{
				{UseHost: true, Host: "tun.local:8443", TunneledHost: "pub.local"},
				{UseHost: false, Host: "nohost.local"},
			}},
		{DeviceName: "c", IP: "192.168.201.3", Hostnames: []string{"shared.local", "both.local"}},
		{DeviceName: "d", IP: "192.168.201.4", Tunnels: []utils.ProxyRouteConfig{{UseHost: true, Host: "both.local"}}},
		{DeviceName: "old", IP: "192.168.201.9"}, // pre-upgrade heartbeat without Hostnames
	}
	lbs := []string{"192.168.201.1"}

	want := map[string]clusterHostname{
		"b.local":      {IPs: []string{"192.168.201.2"}},
		"shared.local": {IPs: []string{"192.168.201.2", "192.168.201.3"}},
		"tun.local":    {IPs: lbs, Tunneled: true},
		"pub.local":    {IPs: lbs, Tunneled: true},
		"both.local":   {IPs: lbs, Tunneled: true},
	}
	if got := buildClusterDNS(heartbeats, lbs); !reflect.DeepEqual(got, want) {
		t.Errorf("buildClusterDNS() = %v, want %v", got, want)
	}

	// plain seen after tunneled must not demote it either
	rev := []NodeHeartbeat{heartbeats[2], heartbeats[1]}
	if got := buildClusterDNS(rev, lbs)["both.local"]; !reflect.DeepEqual(got, want["both.local"]) {
		t.Errorf("reversed both.local = %v, want %v", got, want["both.local"])
	}

	// without a load balancer the advertiser is the entry point
	got := buildClusterDNS(heartbeats[:1], nil)["tun.local"]
	if !reflect.DeepEqual(got, clusterHostname{IPs: []string{"192.168.201.2"}, Tunneled: true}) {
		t.Errorf("no-LB tun.local = %v", got)
	}
}

func TestUnitHandleDNSRequestClusterHostnames(t *testing.T) {
	setupTestEnv(t, func(cfg *utils.Config) {
		cfg.ConstellationConfig.ThisDeviceName = "node-a"
		cfg.HTTPConfig.Hostname = "a.local"
		cfg.HTTPConfig.ProxyConfig.Routes = []utils.ProxyRouteConfig{
			{Name: "mine", UseHost: true, Host: "shared.local"},
		}
	})
	seedDeviceCache(t,
		utils.ConstellationDevice{DeviceName: "node-a", IP: "192.168.201.5"},
		utils.ConstellationDevice{DeviceName: "lb-1", IP: "192.168.201.1", IsLoadBalancer: true},
	)
	seedClusterDNS(t, map[string]clusterHostname{
		"b.local":          {IPs: []string{"192.168.201.2"}},
		"shared.local":     {IPs: []string{"192.168.201.2"}},
		"sub.a.local":      {IPs: []string{"192.168.201.3"}},
		"tun.local":        {IPs: []string{"192.168.201.1"}, Tunneled: true},
		"shared.tun.local": {IPs: []string{"192.168.201.1"}, Tunneled: true},
	})

	tests := []struct {
		qName string
		want  []string
	}{
		{"b.local.", []string{"192.168.201.2"}},         // another node's plain route
		{"www.b.local.", []string{"192.168.201.2"}},     // subdomain match
		{"B.LOCAL.", []string{"192.168.201.2"}},         // case-insensitive
		{"shared.local.", []string{"192.168.201.5"}},    // local wins a tie
		{"sub.a.local.", []string{"192.168.201.3"}},     // more specific cluster beats shorter local
		{"tun.local.", []string{"192.168.201.1"}},       // tunnel advertised elsewhere -> LB
		{"a.local.", []string{"192.168.201.5"}},         // local unchanged
		{"deep.sub.a.local.", []string{"192.168.201.3"}},
	}
	for _, tt := range tests {
		if got := answeredIPs(t, tt.qName); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("DNS answer for %s = %v, want %v", tt.qName, got, tt.want)
		}
	}
}

func TestUnitClusterDNSLookupTunneledWinsTie(t *testing.T) {
	seedClusterDNS(t, map[string]clusterHostname{
		"x.local": {IPs: []string{"192.168.201.1"}, Tunneled: true},
	})
	host, entry := clusterDNSLookup("x.local.")
	if host != "x.local" || !entry.Tunneled {
		t.Errorf("lookup = %q %v", host, entry)
	}
	if host, _ := clusterDNSLookup("nope.local."); host != "" {
		t.Errorf("unexpected match %q", host)
	}
}

func TestUnitTunnelHostOverridden(t *testing.T) {
	tests := []struct {
		host, tunneledHost string
		want               bool
	}{
		{"a.local", "", false},
		{"a.local", "a.local", false},
		{"a.local", "A.LOCAL", false},           // case-insensitive
		{"a.local:8443", "a.local:9000", false}, // ports ignored
		{"a.local", "b.local", true},
		{"a.local", "b.local:8443", true},
		{"a.local", "b c.local", false}, // unusable override is no override
		{"a.local", "b,c.local", false},
	}
	for _, tt := range tests {
		route := utils.ProxyRouteConfig{Host: tt.host, TunneledHost: tt.tunneledHost}
		if got := tunnelHostOverridden(route); got != tt.want {
			t.Errorf("tunnelHostOverridden(%q, %q) = %v, want %v", tt.host, tt.tunneledHost, got, tt.want)
		}
	}
}

// Replays the production scenario: a manager (non-LB) receives an agent's
// heartbeat over the wire and must answer DNS for the agent's plain routes.
func TestUnitHandleDNSRequestFromWireHeartbeat(t *testing.T) {
	setupTestEnv(t, func(cfg *utils.Config) {
		cfg.ConstellationConfig.ThisDeviceName = "cosmos-0"
		cfg.HTTPConfig.Hostname = "cosmos.mocham.com"
	})
	seedDeviceCache(t, utils.ConstellationDevice{DeviceName: "cosmos-0", IP: "192.168.201.1"})

	// verbatim shape of an agent heartbeat as stored in constellation-nodes
	wire := `{"DeviceName":"vpn","IP":"192.168.201.2","IsRelay":true,"IsLighthouse":true,"IsExitNode":true,"CosmosNode":1,"Tunnels":[],"hostnames":["vpn.mocham.com","test-url.com","filebrowser-vpn.mocham.com","159.65.210.47"],"runningDeployments":[],"monitoringOn":false}`
	var hb NodeHeartbeat
	if err := json.Unmarshal([]byte(wire), &hb); err != nil {
		t.Fatal("unmarshal wire heartbeat:", err)
	}
	if len(hb.Hostnames) != 4 {
		t.Fatalf("wire heartbeat decoded %d hostnames, want 4 (json tag drift?)", len(hb.Hostnames))
	}
	seedClusterDNS(t, buildClusterDNS([]NodeHeartbeat{hb}, nil))

	tests := []struct {
		qName string
		want  []string
	}{
		{"filebrowser-vpn.mocham.com.", []string{"192.168.201.2"}},
		{"vpn.mocham.com.", []string{"192.168.201.2"}},
		{"cosmos.mocham.com.", []string{"192.168.201.1"}}, // own hostname stays local
	}
	for _, tt := range tests {
		if got := answeredIPs(t, tt.qName); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("DNS answer for %s = %v, want %v", tt.qName, got, tt.want)
		}
	}
}
