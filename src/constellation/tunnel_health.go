package constellation

import (
	"errors"
	"net"
	"net/url"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/azukaar/cosmos-server/src/docker"
	"github.com/azukaar/cosmos-server/src/utils"
)

const (
	tunnelProbeInterval = 5 * time.Second
	tunnelProbeTimeout  = 2 * time.Second
	// consecutive failed sweeps before a backend is withdrawn — a single blip
	// would otherwise restart the HTTP server of every load balancer twice
	tunnelProbeFailuresToWithdraw = 2
)

type tunnelProbeState struct {
	healthy bool
	fails   int
}

var tunnelHealth = map[string]tunnelProbeState{}
var tunnelHealthMux sync.RWMutex

var tunnelProberStopChan chan struct{}
var tunnelProberLock sync.Mutex

// isTunnelBackendHealthy reports whether the local backend of a tunneled route
// answered the last probes. Unknown routes are healthy: the prober has not run
// yet, or the route has no probeable backend.
func isTunnelBackendHealthy(route utils.ProxyRouteConfig) bool {
	tunnelHealthMux.RLock()
	defer tunnelHealthMux.RUnlock()

	state, exists := tunnelHealth[route.Name]
	if !exists {
		return true
	}
	return state.healthy
}

// tunnelProbeAddress resolves the host:port a tunneled route's backend actually
// listens on, mirroring what the proxy director dials. Returns false when the
// route has no probeable TCP backend (static content, non-http target) or when
// the address cannot be resolved — both fail open.
func tunnelProbeAddress(route utils.ProxyRouteConfig) (string, bool) {
	// static content is served by cosmos itself, a redirect never reaches a backend
	if route.Mode == "STATIC" || route.Mode == "SPA" || route.Mode == "REDIRECT" {
		return "", false
	}

	target, err := url.Parse(route.Target)
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") {
		return "", false
	}

	host := target.Hostname()
	if host == "" {
		return "", false
	}

	port := target.Port()
	if port == "" {
		port = "80"
		if target.Scheme == "https" {
			port = "443"
		}
	}

	// same condition as the proxy director: a SERVAPP hostname is a container
	// name that only Docker can resolve when Cosmos is not on its network
	if route.Mode == "SERVAPP" && (!utils.IsInsideContainer || utils.IsHostNetwork) {
		ip, err := docker.GetContainerIPByName(host)
		if err != nil || ip == "" {
			return "", false
		}
		host = ip
	}

	return net.JoinHostPort(host, port), true
}

// isBackendDownError distinguishes a backend that is unambiguously not
// listening from a probe we could not carry out; anything unclassified fails
// open so a broken prober never withdraws a live backend.
func isBackendDownError(err error) bool {
	if os.IsTimeout(err) || errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
		// a stopped container disappears from Docker's embedded DNS; the proxy
		// would fail to resolve it exactly the same way
		return true
	}
	return false
}

func probeTunnelBackend(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, tunnelProbeTimeout)
	if err == nil {
		conn.Close()
		return true
	}
	return !isBackendDownError(err)
}

// sweepTunnelBackends probes every tunneled route's backend once and rebuilds
// the health cache, dropping routes that no longer exist in the config.
func sweepTunnelBackends() {
	routes := utils.GetMainConfig().HTTPConfig.ProxyConfig.Routes

	type probeResult struct {
		name string
		up   bool
	}
	results := make(chan probeResult, len(routes))
	pending := 0

	for _, route := range routes {
		if route.Tunnel == "" || route.Disabled {
			continue
		}
		addr, probeable := tunnelProbeAddress(route)
		if !probeable {
			continue
		}
		pending++
		go func(name string, addr string) {
			results <- probeResult{name: name, up: probeTunnelBackend(addr)}
		}(route.Name, addr)
	}

	fresh := map[string]bool{}
	for i := 0; i < pending; i++ {
		result := <-results
		fresh[result.name] = result.up
	}

	tunnelHealthMux.Lock()
	defer tunnelHealthMux.Unlock()

	next := map[string]tunnelProbeState{}
	for name, up := range fresh {
		previous := tunnelHealth[name]
		if up {
			next[name] = tunnelProbeState{healthy: true}
			continue
		}
		state := tunnelProbeState{healthy: true, fails: previous.fails + 1}
		if state.fails >= tunnelProbeFailuresToWithdraw {
			state.healthy = false
			if previous.healthy || previous.fails == 0 {
				utils.Warn("[constellation] Tunneled route '" + name + "' backend is not answering, withdrawing it from the constellation")
			}
		}
		next[name] = state
	}
	tunnelHealth = next
}

func StartTunnelProber() {
	StopTunnelProber()

	tunnelProberLock.Lock()
	stopChan := make(chan struct{})
	tunnelProberStopChan = stopChan
	tunnelProberLock.Unlock()

	go func() {
		ticker := time.NewTicker(tunnelProbeInterval)
		defer ticker.Stop()

		sweepTunnelBackends()

		for {
			select {
			case <-stopChan:
				utils.Debug("[constellation] Tunnel backend prober stopped")
				return
			case <-ticker.C:
				sweepTunnelBackends()
			}
		}
	}()
}

func StopTunnelProber() {
	tunnelProberLock.Lock()
	if tunnelProberStopChan != nil {
		close(tunnelProberStopChan)
		tunnelProberStopChan = nil
	}
	tunnelProberLock.Unlock()

	// forget results so a restarted constellation advertises everything until
	// the first sweep lands
	tunnelHealthMux.Lock()
	tunnelHealth = map[string]tunnelProbeState{}
	tunnelHealthMux.Unlock()
}
