//go:build e2e

package constellation

// Tier-A E2E scenarios. Each test provisions a real multi-process
// constellation over loopback (see e2eharness_test.go) and drives it through
// formation, sync, failure and recovery. Scenarios assert *invariants*
// (convergence, connectivity, quorum survival) rather than timing details, so
// they hold across machines; every cluster additionally fails its test if any
// node logged a data race or a panic.

import (
	"fmt"
	"testing"
	"time"
)

// TestE2ENonHAFormation: 1 manager + 2 agents. The agents attach to the
// manager as leafnodes, heartbeats appear in the constellation-nodes KV,
// cross-node messaging works, and the device DB syncs to the agents.
func TestE2ENonHAFormation(t *testing.T) {
	c := newE2ECluster(t, false, []e2eNodeSpec{
		{Name: "prime", Octet: 1, CosmosNode: 2, Lighthouse: true},
		{Name: "agentone", Octet: 2, CosmosNode: 1, Agent: true},
		{Name: "agenttwo", Octet: 3, CosmosNode: 1, Agent: true},
	})

	c.start("prime")
	c.waitConnected("prime", 90*time.Second)

	c.start("agentone")
	c.start("agenttwo")
	c.waitConnected("agentone", 120*time.Second)
	c.waitConnected("agenttwo", 120*time.Second)

	// layer 1: both agents hold a live leafnode link to the manager
	c.waitForDetail(90*time.Second, "manager sees 2 leafnode links", func() (bool, string) {
		st, err := c.node("prime").get("/status")
		if err != nil {
			return false, err.Error()
		}
		leafs, _ := st["leafs"].(float64)
		return int(leafs) == 2, fmt.Sprintf("leafs=%v", st["leafs"])
	})

	// layer 2: cross-node request/reply over the leaf link
	c.waitFor(60*time.Second, "agent ping over NATS", func() bool {
		resp, err := c.node("agentone").post("/request",
			map[string]string{"Topic": "cosmos._global_.ping", "Payload": "Ping"})
		return err == nil && resp["response"] == "Pong"
	})

	// layer 3: heartbeats from every node land in the manager's KV bucket
	// (agents reach the manager's JetStream through the leaf link)
	c.waitForDetail(180*time.Second, "all heartbeats visible on prime", func() (bool, string) {
		names := c.heartbeatNames("prime")
		return containsAll(names, "prime", "agentone", "agenttwo"), fmt.Sprint(names)
	})

	// the device DB replicates to freshly enrolled agents byte-for-byte
	c.waitFor(120*time.Second, "device DB convergence", func() bool {
		h := c.dbHash("prime")
		return h != "" && c.dbHash("agentone") == h && c.dbHash("agenttwo") == h
	})
}

// TestE2EHAFormation: 3 managers with clustered JetStream. The creator alone
// must hold JetStream DOWN (designed formation state: meta-Raft has no
// quorum), a second manager brings it up, and with all three up every manager
// serves JetStream and sees every heartbeat.
func TestE2EHAFormation(t *testing.T) {
	c := newE2ECluster(t, true, []e2eNodeSpec{
		{Name: "prime", Octet: 1, CosmosNode: 2, Lighthouse: true},
		{Name: "mantwo", Octet: 2, CosmosNode: 2},
		{Name: "manthree", Octet: 3, CosmosNode: 2},
	})

	c.start("prime")
	c.waitConnected("prime", 90*time.Second)

	// formation state: alone, the JS meta-group cannot elect a leader
	if js, err := c.node("prime").get("/js"); err == nil && js["ready"] == true {
		t.Error("e2e: JetStream reported ready on a lone HA manager (quorum should require 2)")
	}

	c.start("mantwo")
	c.waitConnected("mantwo", 120*time.Second)
	c.waitFor(180*time.Second, "JetStream quorum with 2 managers", func() bool {
		js, err := c.node("prime").get("/js")
		return err == nil && js["ready"] == true
	})

	c.start("manthree")
	c.waitConnected("manthree", 120*time.Second)

	for _, name := range []string{"prime", "mantwo", "manthree"} {
		name := name
		c.waitFor(180*time.Second, name+" serves JetStream", func() bool {
			js, err := c.node(name).get("/js")
			return err == nil && js["ready"] == true
		})
	}

	c.waitForDetail(180*time.Second, "all heartbeats visible on every manager", func() (bool, string) {
		detail := ""
		ok := true
		for _, name := range []string{"prime", "mantwo", "manthree"} {
			names := c.heartbeatNames(name)
			routes := -2
			if st, err := c.node(name).get("/status"); err == nil {
				if f, isNum := st["routes"].(float64); isNum {
					routes = int(f)
				}
			}
			detail += fmt.Sprintf("%s=%v(routes:%d) ", name, names, routes)
			if !containsAll(names, "prime", "mantwo", "manthree") {
				ok = false
			}
		}
		return ok, detail
	})
}

// TestE2ESyncPropagation: a DB edit on the manager propagates to the agent
// through the JetStream op-log and converges byte-for-byte, with both nodes
// landing on the same log sequence.
func TestE2ESyncPropagation(t *testing.T) {
	c := newE2ECluster(t, false, []e2eNodeSpec{
		{Name: "prime", Octet: 1, CosmosNode: 2, Lighthouse: true},
		{Name: "agentone", Octet: 2, CosmosNode: 1, Agent: true},
	})

	c.start("prime")
	c.waitConnected("prime", 90*time.Second)
	c.start("agentone")
	c.waitConnected("agentone", 120*time.Second)

	// let the initial enrollment sync settle first
	c.waitFor(120*time.Second, "initial DB convergence", func() bool {
		h := c.dbHash("prime")
		return h != "" && c.dbHash("agentone") == h
	})

	if _, err := c.node("prime").post("/edit-device",
		map[string]string{"DeviceName": "agentone", "Nickname": "renamed-by-e2e"}); err != nil {
		t.Fatal("e2e: edit-device:", err)
	}
	// no push step: the edit published one op-log entry, and every node applies
	// it from the stream on its own

	c.waitFor(120*time.Second, "edited nickname visible on agent", func() bool {
		db, err := c.node("agentone").get("/db")
		if err != nil {
			return false
		}
		devices, _ := db["devices"].([]interface{})
		for _, d := range devices {
			dev, _ := d.(map[string]interface{})
			if dev["deviceName"] == "agentone" && dev["nickname"] == "renamed-by-e2e" {
				return true
			}
		}
		return false
	})

	// op-log semantics: converged means same dump AND same position in the log
	c.waitForDetail(60*time.Second, "manager and agent at the same op-log sequence", func() (bool, string) {
		return c.oplogConverged("prime", "agentone")
	})
}

// TestE2EManagerFailoverHA: with 3 managers and 1 agent, killing one manager
// must leave JetStream serving (2/3 quorum), heartbeats flowing, and the
// agent's leafnode failing over to a surviving manager. The killed manager
// then rejoins cleanly.
func TestE2EManagerFailoverHA(t *testing.T) {
	c := newE2ECluster(t, true, []e2eNodeSpec{
		{Name: "prime", Octet: 1, CosmosNode: 2, Lighthouse: true},
		{Name: "mantwo", Octet: 2, CosmosNode: 2},
		{Name: "manthree", Octet: 3, CosmosNode: 2},
		{Name: "agentone", Octet: 4, CosmosNode: 1, Agent: true},
	})

	for _, name := range []string{"prime", "mantwo", "manthree", "agentone"} {
		c.start(name)
		c.waitConnected(name, 180*time.Second)
	}
	c.waitForDetail(180*time.Second, "full formation before failover", func() (bool, string) {
		names := c.heartbeatNames("prime")
		return containsAll(names, "prime", "mantwo", "manthree", "agentone"), fmt.Sprint(names)
	})

	// kill a non-creator manager (crash, not graceful stop)
	c.kill("mantwo")

	c.waitFor(180*time.Second, "JetStream survives on 2/3 quorum", func() bool {
		js, err := c.node("prime").get("/js")
		return err == nil && js["ready"] == true
	})

	// the dead manager's heartbeat must age out of the KV (TTL 10s)
	c.waitFor(120*time.Second, "dead manager heartbeat expires", func() bool {
		names := c.heartbeatNames("prime")
		return len(names) > 0 && !containsAll(names, "mantwo") &&
			containsAll(names, "prime", "manthree")
	})

	// agent messaging still works through surviving managers
	c.waitFor(120*time.Second, "agent messaging after manager loss", func() bool {
		resp, err := c.node("agentone").post("/request",
			map[string]string{"Topic": "cosmos._global_.ping", "Payload": "Ping"})
		return err == nil && resp["response"] == "Pong"
	})

	// rejoin
	c.start("mantwo")
	c.waitConnected("mantwo", 180*time.Second)
	c.waitFor(180*time.Second, "rejoined manager heartbeats again", func() bool {
		return containsAll(c.heartbeatNames("prime"), "prime", "mantwo", "manthree", "agentone")
	})
}

// TestE2EFreezeRecovery: SIGSTOP the only manager (hung process: sockets stay
// open, nothing answers) — the timeout/reconnect paths must recover after
// SIGCONT without a restart.
func TestE2EFreezeRecovery(t *testing.T) {
	c := newE2ECluster(t, false, []e2eNodeSpec{
		{Name: "prime", Octet: 1, CosmosNode: 2, Lighthouse: true},
		{Name: "agentone", Octet: 2, CosmosNode: 1, Agent: true},
	})

	c.start("prime")
	c.waitConnected("prime", 90*time.Second)
	c.start("agentone")
	c.waitConnected("agentone", 120*time.Second)
	c.waitFor(60*time.Second, "messaging before freeze", func() bool {
		resp, err := c.node("agentone").post("/request",
			map[string]string{"Topic": "cosmos._global_.ping", "Payload": "Ping"})
		return err == nil && resp["response"] == "Pong"
	})

	c.freeze("prime")
	time.Sleep(20 * time.Second)

	// while the manager hangs, agent requests must fail (no responders),
	// not hang forever
	done := make(chan bool, 1)
	go func() {
		_, err := c.node("agentone").post("/request",
			map[string]string{"Topic": "cosmos._global_.ping", "Payload": "Ping"})
		done <- (err != nil)
	}()
	select {
	case failed := <-done:
		if !failed {
			t.Error("e2e: request succeeded while the only manager was frozen")
		}
	case <-time.After(30 * time.Second):
		t.Error("e2e: request against frozen manager hung instead of timing out")
	}

	c.thaw("prime")

	c.waitFor(180*time.Second, "messaging recovers after thaw", func() bool {
		resp, err := c.node("agentone").post("/request",
			map[string]string{"Topic": "cosmos._global_.ping", "Payload": "Ping"})
		return err == nil && resp["response"] == "Pong"
	})
	c.waitFor(120*time.Second, "heartbeats resume after thaw", func() bool {
		return containsAll(c.heartbeatNames("prime"), "prime", "agentone")
	})
}

// TestE2ERestartStorm: repeated rapid constellation restarts on the manager
// must not leak goroutines (stacked InitNATSClient supervisors, heartbeat
// loops) and must end in a fully working state.
func TestE2ERestartStorm(t *testing.T) {
	c := newE2ECluster(t, false, []e2eNodeSpec{
		{Name: "prime", Octet: 1, CosmosNode: 2, Lighthouse: true},
		{Name: "agentone", Octet: 2, CosmosNode: 1, Agent: true},
	})

	c.start("prime")
	c.waitConnected("prime", 90*time.Second)
	c.start("agentone")
	c.waitConnected("agentone", 120*time.Second)

	baseline := c.goroutines("prime")

	for i := 0; i < 3; i++ {
		if _, err := c.node("prime").post("/restart", nil); err != nil {
			t.Fatal("e2e: restart:", err)
		}
		time.Sleep(2 * time.Second)
	}

	c.waitConnected("prime", 180*time.Second)
	c.waitFor(180*time.Second, "agent reconnects after restart storm", func() bool {
		resp, err := c.node("agentone").post("/request",
			map[string]string{"Topic": "cosmos._global_.ping", "Payload": "Ping"})
		return err == nil && resp["response"] == "Pong"
	})

	// generous settle, then compare goroutine counts: each restart stacking
	// its supervisor/heartbeat would show up as steady growth
	time.Sleep(15 * time.Second)
	after := c.goroutines("prime")
	if baseline > 0 && after > baseline*2+30 {
		t.Errorf("e2e: goroutines grew from %d to %d after 3 restarts — leak suspected", baseline, after)
	}
}

// goroutines returns the node's current goroutine count, 0 on error.
func (c *e2eCluster) goroutines(name string) int {
	st, err := c.node(name).get("/status")
	if err != nil {
		return 0
	}
	f, _ := st["goroutines"].(float64)
	return int(f)
}
