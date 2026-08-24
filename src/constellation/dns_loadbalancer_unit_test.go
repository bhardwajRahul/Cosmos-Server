package constellation

import (
	"net"
	"reflect"
	"sort"
	"testing"

	"github.com/miekg/dns"

	"github.com/azukaar/cosmos-server/src/utils"
)

// captureDNSWriter is a dns.ResponseWriter that keeps the reply in memory
type captureDNSWriter struct{ msg *dns.Msg }

func (w *captureDNSWriter) LocalAddr() net.Addr {
	return &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 53}
}
func (w *captureDNSWriter) RemoteAddr() net.Addr {
	return &net.UDPAddr{IP: net.IPv4(127, 0, 0, 2), Port: 5353}
}
func (w *captureDNSWriter) WriteMsg(m *dns.Msg) error   { w.msg = m; return nil }
func (w *captureDNSWriter) Write(b []byte) (int, error) { return len(b), nil }
func (w *captureDNSWriter) Close() error                { return nil }
func (w *captureDNSWriter) TsigStatus() error           { return nil }
func (w *captureDNSWriter) TsigTimersOnly(bool)         {}
func (w *captureDNSWriter) Hijack()                     {}

// answeredIPs asks the handler for qName and returns the A records it produced
func answeredIPs(t *testing.T, qName string) []string {
	t.Helper()

	req := new(dns.Msg)
	req.SetQuestion(qName, dns.TypeA)

	w := &captureDNSWriter{}
	handleDNSRequest(w, req)

	if w.msg == nil {
		t.Fatal("handleDNSRequest wrote no reply for " + qName)
	}

	ips := []string{}
	for _, rr := range w.msg.Answer {
		if a, ok := rr.(*dns.A); ok {
			ips = append(ips, a.A.String())
		}
	}
	return ips
}

func tunneledRoutesConfig(cfg *utils.Config) {
	cfg.ConstellationConfig.ThisDeviceName = "node-a"
	cfg.HTTPConfig.Hostname = "cosmos.local:443"
	cfg.HTTPConfig.ProxyConfig.Routes = []utils.ProxyRouteConfig{
		{Name: "lb", UseHost: true, Host: "lbtest.constellation", Tunnel: "_ANY_"},
		{Name: "lb-port", UseHost: true, Host: "ported.constellation:8443", Tunnel: "node-lb"},
		{Name: "aliased", UseHost: true, Host: "private.local", TunneledHost: "public.constellation:8443", Tunnel: "_ANY_"},
		{Name: "plain", UseHost: true, Host: "plain.constellation"},
		{Name: "shared-tunnel", UseHost: true, Host: "both.constellation", Tunnel: "_ANY_"},
		{Name: "shared-local", UseHost: true, Host: "both.constellation"},
		{Name: "no-usehost", UseHost: false, Host: "ignored.constellation", Tunnel: "_ANY_"},
		{Name: "main-tunnel", UseHost: true, Host: "cosmos.local", Tunnel: "_ANY_"},
	}
}

func TestUnitTunneledHostnames(t *testing.T) {
	setupTestEnv(t, tunneledRoutesConfig)
	seedDeviceCache(t, utils.ConstellationDevice{DeviceName: "node-a", IP: "192.168.201.5"})

	want := map[string]bool{
		"lbtest.constellation": true,
		"ported.constellation": true,
		"public.constellation": true,
	}

	if got := tunneledHostnames(); !reflect.DeepEqual(got, want) {
		t.Errorf("tunneledHostnames() = %v, want %v", got, want)
	}
}

func TestUnitTunneledHostnamesEmptyOnLoadBalancer(t *testing.T) {
	setupTestEnv(t, tunneledRoutesConfig)
	seedDeviceCache(t, utils.ConstellationDevice{DeviceName: "node-a", IP: "192.168.201.5", IsLoadBalancer: true})

	if got := tunneledHostnames(); len(got) != 0 {
		t.Errorf("tunneledHostnames() on a load balancer = %v, want empty", got)
	}
}

func TestUnitLoadBalancerIPs(t *testing.T) {
	setupTestEnv(t, nil)
	seedDeviceCache(t,
		utils.ConstellationDevice{DeviceName: "lb-1", IP: "192.168.201.10/24", IsLoadBalancer: true},
		utils.ConstellationDevice{DeviceName: "lb-2", IP: "192.168.201.2", IsLoadBalancer: true},
		utils.ConstellationDevice{DeviceName: "worker", IP: "192.168.201.30"},
		utils.ConstellationDevice{DeviceName: "lb-noip", IP: "", IsLoadBalancer: true},
	)

	// the cache also keys devices by public hostname, so the same LB appears twice
	deviceCacheMux.Lock()
	withAlias := map[string]utils.ConstellationDevice{"lb1.example.com": CachedDevices["lb-1"]}
	for name, device := range CachedDevices {
		withAlias[name] = device
	}
	CachedDevices = withAlias
	deviceCacheMux.Unlock()

	want := []string{"192.168.201.10", "192.168.201.2"}
	sort.Strings(want)

	if got := loadBalancerIPs(); !reflect.DeepEqual(got, want) {
		t.Errorf("loadBalancerIPs() = %v, want %v", got, want)
	}
}

func TestUnitDnsAnswerIPs(t *testing.T) {
	setupTestEnv(t, tunneledRoutesConfig)
	seedDeviceCache(t,
		utils.ConstellationDevice{DeviceName: "node-a", IP: "192.168.201.5"},
		utils.ConstellationDevice{DeviceName: "lb-1", IP: "192.168.201.1", IsLoadBalancer: true},
		utils.ConstellationDevice{DeviceName: "lb-2", IP: "192.168.201.2", IsLoadBalancer: true},
	)

	tunneled := tunneledHostnames()

	tests := []struct {
		hostname string
		want     []string
	}{
		{"lbtest.constellation", []string{"192.168.201.1", "192.168.201.2"}},
		{"public.constellation", []string{"192.168.201.1", "192.168.201.2"}},
		{"plain.constellation", []string{"192.168.201.5"}},
		{"both.constellation", []string{"192.168.201.5"}},
		{"cosmos.local", []string{"192.168.201.5"}},
	}
	for _, tt := range tests {
		if got := dnsAnswerIPs(tt.hostname, "192.168.201.5", tunneled); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("dnsAnswerIPs(%q) = %v, want %v", tt.hostname, got, tt.want)
		}
	}
}

func TestUnitHandleDNSRequestTunneledHostname(t *testing.T) {
	setupTestEnv(t, func(cfg *utils.Config) {
		cfg.ConstellationConfig.ThisDeviceName = "node-a"
		cfg.HTTPConfig.Hostname = "cosmos.local"
		cfg.HTTPConfig.ProxyConfig.Routes = []utils.ProxyRouteConfig{
			{Name: "tunneled", UseHost: true, Host: "app.cosmos.local", Tunnel: "_ANY_"},
			{Name: "local", UseHost: true, Host: "local.cosmos.local"},
		}
	})
	seedDeviceCache(t,
		utils.ConstellationDevice{DeviceName: "node-a", IP: "192.168.201.5"},
		utils.ConstellationDevice{DeviceName: "lb-1", IP: "192.168.201.1", IsLoadBalancer: true},
		utils.ConstellationDevice{DeviceName: "lb-2", IP: "192.168.201.2", IsLoadBalancer: true},
	)

	tests := []struct {
		qName string
		want  []string
	}{
		{"app.cosmos.local.", []string{"192.168.201.1", "192.168.201.2"}},
		{"sub.app.cosmos.local.", []string{"192.168.201.1", "192.168.201.2"}},
		{"local.cosmos.local.", []string{"192.168.201.5"}},
		{"cosmos.local.", []string{"192.168.201.5"}},
	}
	for _, tt := range tests {
		if got := answeredIPs(t, tt.qName); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("DNS answer for %s = %v, want %v", tt.qName, got, tt.want)
		}
	}
}

func TestUnitDnsAnswerIPsFallsBackWithoutLoadBalancer(t *testing.T) {
	setupTestEnv(t, tunneledRoutesConfig)
	seedDeviceCache(t,
		utils.ConstellationDevice{DeviceName: "node-a", IP: "192.168.201.5"},
		utils.ConstellationDevice{DeviceName: "node-b", IP: "192.168.201.6"},
	)

	tunneled := tunneledHostnames()
	want := []string{"192.168.201.5"}

	if got := dnsAnswerIPs("lbtest.constellation", "192.168.201.5", tunneled); !reflect.DeepEqual(got, want) {
		t.Errorf("dnsAnswerIPs with no load balancer known = %v, want %v", got, want)
	}
}

func TestUnitHandleDNSRequestDeviceSuffixAndCase(t *testing.T) {
	setupTestEnv(t, func(cfg *utils.Config) {
		cfg.ConstellationConfig.ThisDeviceName = "node-a"
		cfg.HTTPConfig.Hostname = "cosmos.local"
	})
	seedDeviceCache(t,
		utils.ConstellationDevice{DeviceName: "node-a", IP: "192.168.201.5"},
		utils.ConstellationDevice{DeviceName: "My Laptop", IP: "192.168.201.7"},
	)

	tests := []struct {
		qName string
		want  []string
	}{
		{"my-laptop.", []string{"192.168.201.7"}},
		{"My-Laptop.", []string{"192.168.201.7"}},
		{"my-laptop.constellation.", []string{"192.168.201.7"}},
		{"MY-LAPTOP.CONSTELLATION.", []string{"192.168.201.7"}},
		{"node-a.constellation.", []string{"192.168.201.5"}},
	}
	for _, tt := range tests {
		if got := answeredIPs(t, tt.qName); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("DNS answer for %s = %v, want %v", tt.qName, got, tt.want)
		}
	}
}

func TestUnitHandleDNSRequestCustomWildcard(t *testing.T) {
	setupTestEnv(t, func(cfg *utils.Config) {
		cfg.ConstellationConfig.ThisDeviceName = "node-a"
		cfg.HTTPConfig.Hostname = "cosmos.local"
		cfg.ConstellationConfig.CustomDNSEntries = []utils.ConstellationDNSEntry{
			{Key: "*.example.com", Value: "10.0.0.1"},
			{Key: "app.example.com", Value: "10.0.0.2"},
			{Key: "ads*.net", Value: "10.0.0.3"},
			{Key: "plain.org", Value: "10.0.0.4"},
		}
	})
	seedDeviceCache(t, utils.ConstellationDevice{DeviceName: "node-a", IP: "192.168.201.5"})

	tests := []struct {
		qName string
		want  []string
	}{
		{"foo.example.com.", []string{"10.0.0.1"}},
		{"deep.foo.example.com.", []string{"10.0.0.1"}},
		{"FOO.EXAMPLE.COM.", []string{"10.0.0.1"}},
		{"app.example.com.", []string{"10.0.0.2"}},
		{"ads1.net.", []string{"10.0.0.3"}},
		{"adserver.tracking.net.", []string{"10.0.0.3"}},
		{"sub.plain.org.", []string{"10.0.0.4"}},
	}
	for _, tt := range tests {
		if got := answeredIPs(t, tt.qName); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("DNS answer for %s = %v, want %v", tt.qName, got, tt.want)
		}
	}
}

// answeredMsg asks the handler for qName and returns the raw reply
func answeredMsg(t *testing.T, qName string) *dns.Msg {
	t.Helper()

	req := new(dns.Msg)
	req.SetQuestion(qName, dns.TypeA)

	w := &captureDNSWriter{}
	handleDNSRequest(w, req)

	if w.msg == nil {
		t.Fatal("handleDNSRequest wrote no reply for " + qName)
	}
	return w.msg
}

// deadDNSFallback: a query that wrongly leaves the handler fails fast as
// SERVFAIL instead of reaching a real resolver
const deadDNSFallback = "127.0.0.1:1"

// overriddenTunnelConfig mirrors the two-server scenario: this node is the
// tunnel origin, the exit serves the routes under an overridden TunneledHost
func overriddenTunnelConfig(cfg *utils.Config) {
	cfg.ConstellationConfig.ThisDeviceName = "node-a"
	cfg.ConstellationConfig.DNSFallback = deadDNSFallback
	cfg.HTTPConfig.Hostname = "cosmos.local"
	cfg.HTTPConfig.ProxyConfig.Routes = []utils.ProxyRouteConfig{
		{Name: "aliased", UseHost: true, Host: "no-tunnel.domain.com", TunneledHost: "tunnel.domain.com", Tunnel: "node-lb"},
		{Name: "aliased-port", UseHost: true, Host: "private2.domain.com", TunneledHost: "tunnel2.domain.com:8443", Tunnel: "_ANY_"},
		{Name: "plain", UseHost: true, Host: "plain.domain.com"},
	}
}

func TestUnitHandleDNSRequestOverriddenTunnelHostname(t *testing.T) {
	setupTestEnv(t, overriddenTunnelConfig)
	seedDeviceCache(t,
		utils.ConstellationDevice{DeviceName: "node-a", IP: "192.168.201.5"},
		utils.ConstellationDevice{DeviceName: "node-lb", IP: "192.168.201.2", IsLoadBalancer: true},
	)

	tests := []struct {
		qName string
		want  []string
	}{
		// the origin keeps serving the route's original Host itself
		{"no-tunnel.domain.com.", []string{"192.168.201.5"}},
		{"private2.domain.com.", []string{"192.168.201.5"}},
		{"plain.domain.com.", []string{"192.168.201.5"}},
		// the overridden TunneledHost must resolve to the load balancer from
		// local config alone — no heartbeat or cluster data is seeded here
		{"tunnel.domain.com.", []string{"192.168.201.2"}},
		{"sub.tunnel.domain.com.", []string{"192.168.201.2"}},
		{"TUNNEL.DOMAIN.COM.", []string{"192.168.201.2"}},
		{"tunnel2.domain.com.", []string{"192.168.201.2"}},
	}
	for _, tt := range tests {
		if got := answeredIPs(t, tt.qName); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("DNS answer for %s = %v, want %v", tt.qName, got, tt.want)
		}
	}
}

func TestUnitOverriddenTunnelHostNotForwarded(t *testing.T) {
	setupTestEnv(t, overriddenTunnelConfig)
	seedDeviceCache(t,
		utils.ConstellationDevice{DeviceName: "node-a", IP: "192.168.201.5"},
		utils.ConstellationDevice{DeviceName: "node-lb", IP: "192.168.201.2", IsLoadBalancer: true},
	)

	msg := answeredMsg(t, "tunnel.domain.com.")
	if !msg.Authoritative || msg.Rcode != dns.RcodeSuccess || len(msg.Answer) == 0 {
		t.Errorf("tunnel.domain.com. was forwarded upstream instead of answered locally: rcode=%d authoritative=%v answers=%d",
			msg.Rcode, msg.Authoritative, len(msg.Answer))
	}
}

func TestUnitHandleDNSRequestUnknownForwarded(t *testing.T) {
	// sanity check on the harness: an unknown name really is forwarded, and the
	// dead fallback turns that into SERVFAIL — so empty answers above mean
	// "left the handler", not "answered empty"
	setupTestEnv(t, overriddenTunnelConfig)
	seedDeviceCache(t, utils.ConstellationDevice{DeviceName: "node-a", IP: "192.168.201.5"})

	msg := answeredMsg(t, "unrelated.example.org.")
	if msg.Rcode != dns.RcodeServerFailure {
		t.Errorf("expected unknown name to be forwarded (dead fallback -> SERVFAIL), got rcode=%d answers=%d", msg.Rcode, len(msg.Answer))
	}
}

func TestUnitHandleDNSRequestOverriddenTunnelNoLBFallback(t *testing.T) {
	// no load balancer known yet: answer this node rather than leak upstream
	setupTestEnv(t, overriddenTunnelConfig)
	seedDeviceCache(t, utils.ConstellationDevice{DeviceName: "node-a", IP: "192.168.201.5"})

	want := []string{"192.168.201.5"}
	if got := answeredIPs(t, "tunnel.domain.com."); !reflect.DeepEqual(got, want) {
		t.Errorf("DNS answer for tunnel.domain.com. with no LB = %v, want %v", got, want)
	}
}

func TestUnitTunneledHostnamesOverrideEdgeCases(t *testing.T) {
	setupTestEnv(t, func(cfg *utils.Config) {
		cfg.ConstellationConfig.ThisDeviceName = "node-a"
		cfg.HTTPConfig.Hostname = "cosmos.local"
		cfg.HTTPConfig.ProxyConfig.Routes = []utils.ProxyRouteConfig{
			// override colliding with a host served locally: local wins
			{Name: "local-app", UseHost: true, Host: "app.local"},
			{Name: "collide", UseHost: true, Host: "origin1.local", TunneledHost: "app.local", Tunnel: "_ANY_"},
			// TunneledHost differing only by case/port is not an override
			{Name: "same", UseHost: true, Host: "same.local", TunneledHost: "SAME.local:8443", Tunnel: "_ANY_"},
		}
	})
	seedDeviceCache(t, utils.ConstellationDevice{DeviceName: "node-a", IP: "192.168.201.5"})

	want := map[string]bool{"same.local": true}
	if got := tunneledHostnames(); !reflect.DeepEqual(got, want) {
		t.Errorf("tunneledHostnames() = %v, want %v", got, want)
	}
}
