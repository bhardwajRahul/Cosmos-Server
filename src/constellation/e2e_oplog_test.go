//go:build e2e

package constellation

// M2 op-log scenarios. Written by the test gate rather than the author of the
// apply loop: a test written from the same mental model as the implementation
// shares its blind spots, and the pre-image and rejection paths below are
// exactly where that would bite.

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// nebulaPid reads the node's nebula sub-process id; a restart replaces it.
func (c *e2eCluster) nebulaPid(name string) int {
	st, err := c.node(name).get("/status")
	if err != nil {
		return -1
	}
	pid, ok := st["nebulaPid"].(float64)
	if !ok {
		return -1
	}
	return int(pid)
}

// waitConverged fails the test with per-node epoch/seq/hash detail rather than
// a bare boolean, so a convergence failure is diagnosable from the log alone.
func (c *e2eCluster) waitConverged(d time.Duration, what string, names ...string) {
	c.t.Helper()
	c.waitForDetail(d, what, func() (bool, string) {
		return c.oplogConverged(names...)
	})
}

// deviceNames returns the node's view of the device table, keyed by name.
func (c *e2eCluster) deviceView(node string) map[string]map[string]interface{} {
	out := map[string]map[string]interface{}{}
	db, err := c.node(node).get("/db")
	if err != nil {
		return out
	}
	devices, _ := db["devices"].([]interface{})
	for _, d := range devices {
		dev, _ := d.(map[string]interface{})
		if n, ok := dev["deviceName"].(string); ok {
			out[n] = dev
		}
	}
	return out
}

// settle waits for the node's nebula pid to hold steady, so a later pid
// comparison can't be confounded by an enrollment restart still in flight.
// (Learned the hard way: sampling before the mesh quiesces misattributes an
// enrollment bounce to whatever write happened next.)
func (c *e2eCluster) settleNebula(node string, stable time.Duration, limit time.Duration) int {
	c.t.Helper()
	pid := -1
	since := time.Time{}
	c.waitForDetail(limit, "nebula pid stable on "+node, func() (bool, string) {
		cur := c.nebulaPid(node)
		if cur <= 0 {
			pid, since = -1, time.Time{}
			return false, fmt.Sprintf("%s pid=%d", node, cur)
		}
		if cur != pid {
			pid, since = cur, time.Now()
			return false, fmt.Sprintf("%s pid moved to %d", node, cur)
		}
		return time.Since(since) >= stable, fmt.Sprintf("%s pid=%d steady %s", node, cur, time.Since(since).Truncate(time.Second))
	})
	return pid
}

// TestE2EOplogPropagation: one op of every kind published from one node must
// land on every other node, with all nodes agreeing on (epoch, seq) — not just
// on content. Equal dumps with unequal seq would mean divergent logs that
// happen to look the same right now.
// waitWritable blocks until every named node reports a writable op-log. Writes
// racing a designed detach window (post-reaction RestartNebula, post-install
// bounce) get a correct 409 — a scenario that means to test replication rather
// than the window must wait it out first.
func (c *e2eCluster) waitWritable(d time.Duration, names ...string) {
	c.t.Helper()
	for _, n := range names {
		node := n
		c.waitForDetail(d, "op-log writable on "+node, func() (bool, string) {
			st, err := c.node(node).get("/oplog")
			if err != nil {
				return false, err.Error()
			}
			w, _ := st["configWritable"].(bool)
			halted, _ := st["halted"].(bool)
			return w && !halted, fmt.Sprint("writable=", w, " halted=", halted)
		})
	}
}

func TestE2EOplogPropagation(t *testing.T) {
	c := newE2ECluster(t, false, []e2eNodeSpec{
		{Name: "prime", Octet: 1, CosmosNode: 2, Lighthouse: true},
		{Name: "agentone", Octet: 2, CosmosNode: 1, Agent: true},
	})

	c.start("prime")
	c.waitConnected("prime", 90*time.Second)
	c.start("agentone")
	c.waitConnected("agentone", 120*time.Second)
	c.waitConverged(150*time.Second, "initial convergence", "prime", "agentone")
	c.waitWritable(90*time.Second, "prime")

	// kind=db, table=devices
	if _, err := c.node("prime").post("/create-device",
		map[string]string{"DeviceName": "oplog-dev", "IP": "192.168.201.90"}); err != nil {
		t.Fatal("e2e: create-device:", err)
	}
	// the device insert's reaction bounces nebula (and detaches the loop) on every
	// node; wait out prime's own window before the next write
	c.waitWritable(90*time.Second, "prime")
	// kind=db, table=users (PRIMARY KEY table — different code path to devices)
	if _, err := c.node("prime").post("/create-user",
		map[string]string{"Nickname": "oplog-user"}); err != nil {
		t.Fatal("e2e: create-user:", err)
	}
	// kind=domain
	if _, err := c.node("prime").post("/domain-op",
		map[string]interface{}{"Tokens": []string{"tok-a", "tok-b"}}); err != nil {
		t.Fatal("e2e: domain-op:", err)
	}

	c.waitConverged(120*time.Second, "all op kinds converged", "prime", "agentone")

	if _, ok := c.deviceView("agentone")["oplog-dev"]; !ok {
		t.Error("device op did not reach agentone")
	}

	// the domain op needs its own read side: converged dumps only cover SQLite,
	// and domain state lives in config.json
	c.waitForDetail(60*time.Second, "domain op visible on agentone", func() (bool, string) {
		st, err := c.node("agentone").get("/domain-state")
		if err != nil {
			return false, err.Error()
		}
		toks, _ := st["apiTokens"].([]interface{})
		return len(toks) == 2, fmt.Sprint("apiTokens=", toks)
	})
}

// TestE2EOplogAgentWrite: an agent's write goes out over its leafnode to the
// manager's JetStream and comes back through the log like any other. Agents
// being uniform with managers is a load-bearing M2 claim.
func TestE2EOplogAgentWrite(t *testing.T) {
	c := newE2ECluster(t, false, []e2eNodeSpec{
		{Name: "prime", Octet: 1, CosmosNode: 2, Lighthouse: true},
		{Name: "agentone", Octet: 2, CosmosNode: 1, Agent: true},
	})

	c.start("prime")
	c.waitConnected("prime", 90*time.Second)
	c.start("agentone")
	c.waitConnected("agentone", 120*time.Second)
	c.waitConverged(150*time.Second, "initial convergence", "prime", "agentone")
	// the agent may still be inside its own post-snapshot-install mesh bounce;
	// its ladder correctly answers read-only until it re-attaches
	c.waitWritable(90*time.Second, "agentone", "prime")

	if _, err := c.node("agentone").post("/create-device",
		map[string]string{"DeviceName": "from-agent", "IP": "192.168.201.91"}); err != nil {
		t.Fatal("e2e: agent create-device:", err)
	}

	c.waitConverged(120*time.Second, "agent write converged", "prime", "agentone")

	if _, ok := c.deviceView("prime")["from-agent"]; !ok {
		t.Error("agent-originated write never reached the manager")
	}
}

// TestE2EOplogConflictingWrites: two nodes race the same device IP and the same
// user nickname. Exactly one of each must win everywhere, and the loser must be
// rejected with 409 duplicate rather than halting anyone's apply loop.
//
// Both paths matter and they are NOT the same code: devices reject through
// SQLITE_CONSTRAINT_UNIQUE (partial indexes) and users through
// SQLITE_CONSTRAINT_PRIMARYKEY (nickname PK). A classification that handles
// only UNIQUE turns a duplicate user into an unrecognized error and halts the
// apply loop on every node — a cluster-wide stop from a routine name clash.
func TestE2EOplogConflictingWrites(t *testing.T) {
	c := newE2ECluster(t, false, []e2eNodeSpec{
		{Name: "prime", Octet: 1, CosmosNode: 2, Lighthouse: true},
		{Name: "agentone", Octet: 2, CosmosNode: 1, Agent: true},
	})

	c.start("prime")
	c.waitConnected("prime", 90*time.Second)
	c.start("agentone")
	c.waitConnected("agentone", 120*time.Second)
	c.waitConverged(150*time.Second, "initial convergence", "prime", "agentone")

	race := func(path string, body interface{}) (okCount, dupCount int, loserCodes []string, statuses []string) {
		var mu sync.Mutex
		var wg sync.WaitGroup
		for _, node := range []string{"prime", "agentone"} {
			wg.Add(1)
			go func(n string) {
				defer wg.Done()
				out, err := c.node(n).post(path, body)
				mu.Lock()
				defer mu.Unlock()
				code, _ := out["code"].(string)
				status, _ := out["status"].(float64)
				statuses = append(statuses, fmt.Sprintf("%s=%v/%v(err=%v)", n, status, code, err))
				switch {
				case err == nil:
					okCount++
				case code == "duplicate":
					dupCount++
					loserCodes = append(loserCodes, code)
				default:
					loserCodes = append(loserCodes, code)
				}
			}(node)
		}
		wg.Wait()
		return
	}

	// A loser may legitimately lose to the index OR to a bounce window, but not
	// to anything else: an internal/not-found would mean a duplicate stopped
	// being classified, which is the regression that halts apply loops.
	assertLoserCodes := func(what string, codes []string) {
		t.Helper()
		for _, code := range codes {
			switch code {
			case "duplicate", "read-only", "apply-timeout":
			default:
				t.Errorf("%s: loser returned code %q, want duplicate/read-only/apply-timeout", what, code)
			}
		}
	}

	// devices: UNIQUE(ip) WHERE blocked=0. The winner's apply bounces nebula on
	// BOTH nodes mid-race, so the loser may see read-only/timeout instead of the
	// duplicate rejection — all are correct outcomes here. The invariants that
	// must hold: exactly one winner, never two, and nobody halts (checked below).
	c.waitWritable(90*time.Second, "prime", "agentone")
	okD, dupD, codesD, stD := race("/create-device", map[string]string{"DeviceName": "racer", "IP": "192.168.201.92"})
	if okD != 1 {
		t.Errorf("device race: want exactly 1 winner, got ok=%d dup=%d (%v)", okD, dupD, stD)
	}
	assertLoserCodes("device race", codesD)
	if dupD == 0 {
		t.Logf("device race: loser lost to the reaction window rather than the index (%v)", stD)
	}

	// users: PRIMARY KEY(nickname) — a different SQLite code reaching the same
	// rejection; this is the one that halts the loop if misclassified. User ops
	// have no reaction, so no window opens mid-race: strict 1-winner-1-duplicate.
	c.waitWritable(90*time.Second, "prime", "agentone")
	okU, dupU, codesU, stU := race("/create-user", map[string]string{"Nickname": "racer-user"})
	if okU != 1 || dupU != 1 {
		t.Errorf("user race: want exactly 1 winner and 1 duplicate, got ok=%d dup=%d (%v)", okU, dupU, stU)
	}
	assertLoserCodes("user race", codesU)

	// the decisive part: a rejection must commit its seq and leave every loop
	// running, so the cluster still converges afterwards
	c.waitConverged(120*time.Second, "converged after conflicting writes", "prime", "agentone")

	for _, n := range []string{"prime", "agentone"} {
		st, err := c.node(n).get("/oplog")
		if err != nil {
			t.Fatalf("e2e: /oplog on %s: %v", n, err)
		}
		if halted, _ := st["halted"].(bool); halted {
			t.Errorf("%s apply loop HALTED after a duplicate: %v", n, st["haltReason"])
		}
	}
}

// TestE2EOplogDeviceReactionWiring pins the apply loop's pre-image diff in BOTH
// directions, which is the only way to catch it.
//
// A Tags-only edit must not bounce nebula; an IP change must. Asserting only
// the first is worse than useless: if ApplyOpTx hands the reaction an empty
// pre-image, DeviceFieldsChanged returns false for everything, every topology
// change silently degrades to a cache refresh, the mesh never reconverges — and
// a pid-stability-only test goes GREEN precisely when the system is broken.
func TestE2EOplogDeviceReactionWiring(t *testing.T) {
	c := newE2ECluster(t, false, []e2eNodeSpec{
		{Name: "prime", Octet: 1, CosmosNode: 2, Lighthouse: true},
		{Name: "agentone", Octet: 2, CosmosNode: 1, Agent: true},
	})

	c.start("prime")
	c.waitConnected("prime", 90*time.Second)
	c.start("agentone")
	c.waitConnected("agentone", 120*time.Second)
	c.waitConverged(150*time.Second, "initial convergence", "prime", "agentone")

	if _, err := c.node("prime").post("/create-device",
		map[string]string{"DeviceName": "reactor", "IP": "192.168.201.93"}); err != nil {
		t.Fatal("e2e: create-device:", err)
	}
	c.waitConverged(120*time.Second, "reactor device converged", "prime", "agentone")

	before := c.settleNebula("agentone", 10*time.Second, 180*time.Second)
	if before <= 0 {
		t.Fatalf("e2e: agentone has no nebula process (pid %d); both assertions below would be vacuous", before)
	}

	// --- direction 1: Tags-only edit must NOT restart nebula
	if _, err := c.node("prime").post("/set-device-fields",
		map[string]interface{}{"DeviceName": "reactor", "Tags": []string{"prod", "eu"}}); err != nil {
		t.Fatal("e2e: set tags:", err)
	}
	c.waitConverged(120*time.Second, "tags edit converged", "prime", "agentone")
	time.Sleep(8 * time.Second) // a bounce provoked by this op would land by now

	afterTags := c.nebulaPid("agentone")
	if afterTags != before {
		t.Errorf("Tags-only edit bounced nebula on agentone: pid %d -> %d (pre-image diff classified Tags as topology)", before, afterTags)
	}

	// --- direction 2: IP change MUST restart nebula.
	// This is the load-bearing half: it fails if the pre-image is empty, which
	// is the failure mode direction 1 cannot see.
	if _, err := c.node("prime").post("/set-device-fields",
		map[string]interface{}{"DeviceName": "reactor", "IP": "192.168.201.94"}); err != nil {
		t.Fatal("e2e: set ip:", err)
	}
	c.waitConverged(120*time.Second, "ip edit converged", "prime", "agentone")

	restarted := false
	c.waitForDetail(90*time.Second, "IP change restarts nebula on agentone", func() (bool, string) {
		cur := c.nebulaPid("agentone")
		if cur > 0 && cur != afterTags {
			restarted = true
		}
		return restarted, fmt.Sprintf("pid %d -> %d", afterTags, cur)
	})
	if !restarted {
		t.Errorf("IP change did NOT restart nebula on agentone (pid stayed %d) — topology change silently degraded to a cache refresh; the mesh would never reconverge", afterTags)
	}
}
