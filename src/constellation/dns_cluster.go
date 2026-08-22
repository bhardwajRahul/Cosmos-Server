package constellation

import (
	"strings"
	"sync"

	"github.com/azukaar/cosmos-server/src/utils"
)

// clusterHostname is what the cluster knows about a hostname served by some
// node: tunneled names point at the load balancers, plain names at the node.
type clusterHostname struct {
	IPs      []string
	Tunneled bool
}

var clusterDNS = map[string]clusterHostname{}
var clusterDNSMutex sync.RWMutex

func usableHost(host string) bool {
	return host != "" && !strings.Contains(host, ",") && !strings.Contains(host, " ")
}

// localHostnames lists the names this node serves itself (main hostname and
// non-tunneled routes), advertised in the heartbeat so every DNS can answer
func localHostnames() []string {
	config := utils.GetMainConfig()
	seen := map[string]bool{}
	names := []string{}

	add := func(host string) {
		host = hostOnly(host)
		if !usableHost(host) || seen[host] {
			return
		}
		seen[host] = true
		names = append(names, host)
	}

	add(config.HTTPConfig.Hostname)
	for _, route := range config.HTTPConfig.ProxyConfig.Routes {
		if route.UseHost && route.Tunnel == "" {
			add(route.Host)
		}
	}

	return names
}

// buildClusterDNS merges every node's advertised hostnames. A tunneled name
// wins over the same plain name; IPs are deduplicated and sorted so answers
// stay stable across refreshes.
func buildClusterDNS(heartbeats []NodeHeartbeat, lbIPs []string) map[string]clusterHostname {
	result := map[string]clusterHostname{}

	add := func(host string, tunneled bool, ips []string) {
		host = hostOnly(host)
		if !usableHost(host) {
			return
		}
		entry := result[host]
		if tunneled && !entry.Tunneled {
			entry = clusterHostname{Tunneled: true}
		} else if !tunneled && entry.Tunneled {
			return
		}
		for _, ip := range ips {
			entry.IPs = appendUniqueIP(entry.IPs, ip)
		}
		result[host] = entry
	}

	for _, hb := range heartbeats {
		for _, host := range hb.Hostnames {
			add(host, false, []string{hb.IP})
		}
		for _, route := range hb.Tunnels {
			if !route.UseHost {
				continue
			}
			// no load balancer yet: the advertiser is the only way in
			ips := lbIPs
			if len(ips) == 0 {
				ips = []string{hb.IP}
			}
			add(route.Host, true, ips)
			if route.TunneledHost != "" {
				add(route.TunneledHost, true, ips)
			}
		}
	}

	return result
}

func appendUniqueIP(ips []string, ip string) []string {
	ip = cleanIp(ip)
	if ip == "" {
		return ips
	}
	i := 0
	for i < len(ips) && ips[i] < ip {
		i++
	}
	if i < len(ips) && ips[i] == ip {
		return ips
	}
	ips = append(ips, "")
	copy(ips[i+1:], ips[i:])
	ips[i] = ip
	return ips
}

func setClusterDNS(m map[string]clusterHostname) {
	clusterDNSMutex.Lock()
	clusterDNS = m
	clusterDNSMutex.Unlock()
}

// clusterDNSLookup returns the most specific cluster hostname matching qName
func clusterDNSLookup(qName string) (string, clusterHostname) {
	clusterDNSMutex.RLock()
	defer clusterDNSMutex.RUnlock()

	best := ""
	var bestEntry clusterHostname
	for host, entry := range clusterDNS {
		if !matchesDomain(qName, host) {
			continue
		}
		if len(host) > len(best) || (len(host) == len(best) && entry.Tunneled && !bestEntry.Tunneled) {
			best, bestEntry = host, entry
		}
	}
	return best, bestEntry
}
