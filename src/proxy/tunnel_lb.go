package proxy

import (
	"sync"
	"sync/atomic"
	"strings"

	"github.com/azukaar/cosmos-server/src/constellation"
	"github.com/azukaar/cosmos-server/src/utils"
)

type routeCounter struct {
	val atomic.Uint64
}

type TunnelLoadBalancer struct {
	counters sync.Map // routeName -> *routeCounter
}

func (lb *TunnelLoadBalancer) getCounter(routeName string) *routeCounter {
	if v, ok := lb.counters.Load(routeName); ok {
		return v.(*routeCounter)
	}
	c := &routeCounter{}
	actual, _ := lb.counters.LoadOrStore(routeName, c)
	return actual.(*routeCounter)
}

// targetLoad carries a candidate's latest heartbeat resource sample.
type targetLoad struct {
	cpu, ram  float64
	monitored bool
}

func (lb *TunnelLoadBalancer) roundRobin(keys []string, routeName string) string {
	c := lb.getCounter(routeName)
	idx := c.val.Add(1) - 1
	return keys[idx%uint64(len(keys))]
}

// leastBusy picks the key with the lowest max(cpu, ram). Same all-or-nothing
// rule as pro's LeastBusyPlacement: if any candidate lacks trustworthy
// metrics, the whole decision falls back to round-robin.
func (lb *TunnelLoadBalancer) leastBusy(keys []string, routeName string, loads map[string]targetLoad) string {
	best := ""
	bestScore := 0.0
	for _, k := range keys {
		l, ok := loads[k]
		if !ok || !l.monitored {
			return lb.roundRobin(keys, routeName)
		}
		score := l.cpu
		if l.ram > score {
			score = l.ram
		}
		if best == "" || score < bestScore {
			best, bestScore = k, score
		}
	}
	return best
}

// Select picks a key from the given list using the configured LB mode.
// Keys can be numeric indices ("0","1") for regular routes or device names for constellation tunnels.
// "load_based" needs per-key metrics; without them (this path) it degrades to round-robin.
func (lb *TunnelLoadBalancer) Select(keys []string, routeName string, mode string, sticky bool, stickyKey string) string {
	return lb.selectKey(keys, routeName, mode, sticky, stickyKey, nil)
}

func (lb *TunnelLoadBalancer) selectKey(keys []string, routeName string, mode string, sticky bool, stickyKey string, loads map[string]targetLoad) string {
	if len(keys) == 0 {
		return ""
	}

	// Check sticky assignment
	if sticky && stickyKey != "" {
		prev, ok := constellation.GetStickyTarget(stickyKey)
		if ok {
			for _, k := range keys {
				if k == prev {
					return k
				}
			}
			// Sticky target gone from list, fall through to re-assign
		}
	}

	// Mode selection
	var selected string
	switch strings.ToLower(mode) {
	case "round_robin":
		selected = lb.roundRobin(keys, routeName)
	case "load_based":
		selected = lb.leastBusy(keys, routeName, loads)
	case "", "first":
		selected = keys[0]
	default:
		// only reachable via a hand-edited config file, the API rejects unknown modes
		utils.Warn("TunnelLoadBalancer: unknown lb_mode \"" + mode + "\" on route " + routeName + ", load balancing is off (always using the first target)")
		selected = keys[0]
	}

	// Store sticky assignment
	if sticky && stickyKey != "" {
		constellation.SetStickyTarget(stickyKey, selected)
	}

	return selected
}

// SelectTarget is a convenience wrapper for constellation tunnel targets.
func (lb *TunnelLoadBalancer) SelectTarget(targets []utils.TunnelTarget, routeName string, mode string, sticky bool, stickyKey string) *utils.TunnelTarget {
	if len(targets) == 0 {
		return nil
	}

	keys := make([]string, len(targets))
	loads := make(map[string]targetLoad, len(targets))
	for i, t := range targets {
		keys[i] = t.DeviceName
		loads[t.DeviceName] = targetLoad{cpu: t.CPUPercent, ram: t.RAMPercent, monitored: t.MonitoringOn}
	}

	key := lb.selectKey(keys, routeName, mode, sticky, stickyKey, loads)
	if key == "" {
		return nil
	}

	for i := range targets {
		if targets[i].DeviceName == key {
			return &targets[i]
		}
	}
	return nil
}

var DefaultTunnelLB = &TunnelLoadBalancer{}
