//go:build e2e

package constellation

// M3: force-reform and the fencing that has to hold around it.
//
// Reform is the one operation that deliberately splits a constellation's
// history in two, and every guarantee it makes is about what the ABANDONED half
// can no longer do. So these scenarios spend most of their assertions on the
// losing side: a survivor that can write again is easy, a stale node that can
// still be heard is the failure that matters and the one that looks like
// nothing is wrong.

import (
	"fmt"
	"testing"
	"time"
)

// oplogState is the node's full op-log position and formation state in one read,
// so a failure message can say which of the three it was that disagreed.
type oplogState struct {
	epoch           uint64
	seq             int64
	attached        bool
	halted          bool
	formationWriter bool
	streamSeen      bool
	configWritable  bool
	err             error
}

func (c *e2eCluster) oplogState(node string) oplogState {
	st, err := c.node(node).get("/oplog")
	if err != nil {
		return oplogState{err: err, seq: -1}
	}
	epoch, _ := st["epoch"].(float64)
	seq, _ := st["seq"].(float64)
	attached, _ := st["attached"].(bool)
	halted, _ := st["halted"].(bool)
	fw, _ := st["formationWriter"].(bool)
	seen, _ := st["streamSeen"].(bool)
	writable, _ := st["configWritable"].(bool)
	return oplogState{
		epoch: uint64(epoch), seq: int64(seq), attached: attached, halted: halted,
		formationWriter: fw, streamSeen: seen, configWritable: writable,
	}
}

func (s oplogState) String() string {
	if s.err != nil {
		return "unreachable(" + s.err.Error() + ")"
	}
	return fmt.Sprintf("e%d/%d attached=%v formationWriter=%v streamSeen=%v writable=%v halted=%v",
		s.epoch, s.seq, s.attached, s.formationWriter, s.streamSeen, s.configWritable, s.halted)
}

// forceReform triggers the product's own force-reform handler on the node and
// returns the epoch it reports. The handler answers in the standard API envelope
// ({status, data:{...}}), so the epoch is unwrapped from data rather than read
// off the top level.
func (c *e2eCluster) forceReform(node string) uint64 {
	c.t.Helper()
	out, err := c.node(node).post("/force-reform", map[string]interface{}{})
	if err != nil {
		c.t.Fatalf("e2e: force-reform on %s: %v", node, err)
	}
	data, ok := out["data"].(map[string]interface{})
	if !ok {
		c.t.Fatalf("e2e: force-reform on %s returned no data envelope: %v", node, out)
	}
	epoch, ok := data["epoch"].(float64)
	if !ok || epoch == 0 {
		c.t.Fatalf("e2e: force-reform on %s reported no epoch: %v", node, data)
	}
	c.t.Logf("e2e: force-reform on %s -> epoch %d, blocked %v", node, uint64(epoch), data["blockedDevices"])
	return uint64(epoch)
}

// waitFormed blocks until every named node has finished formation: a log exists
// for its epoch, it is attached to it, and it no longer holds the direct-write
// licence.
//
// This is a stricter precondition than convergence or writability, and both of
// those are too weak to start a reform scenario from. An HA cluster mid-formation
// converges perfectly well at seq 0 — the formation writer serves its store to
// the others by snapshot, so every node reports the same dump at the same
// position — and it reports itself writable, because the formation licence IS a
// form of writability. Sampling there and calling it "a healthy cluster" starts
// the scenario in the state the scenario is supposed to create.
func (c *e2eCluster) waitFormed(d time.Duration, names ...string) {
	c.t.Helper()
	for _, n := range names {
		node := n
		c.waitForDetail(d, "formation complete on "+node, func() (bool, string) {
			st := c.oplogState(node)
			return st.attached && !st.formationWriter && !st.halted, node + ": " + st.String()
		})
	}
}

// waitReadOnly blocks until the node refuses writes, and returns the number of
// attempts it took. Used to establish the pre-reform state: reform is only
// meaningful from a manager that has genuinely lost its quorum.
func (c *e2eCluster) waitReadOnly(d time.Duration, node, prefix string) {
	c.t.Helper()
	n := 0
	c.waitForDetail(d, node+" refuses writes", func() (bool, string) {
		n++
		code, _ := c.tryWrite(node, fmt.Sprintf("%s-%d", prefix, n))
		return code == "read-only", fmt.Sprintf("attempt %d -> %q", n, code)
	})
}

// TestE2EForceReform: three managers and an agent, two managers lost for good.
// The survivor must be able to reform, write again while no log exists, take on
// a replacement manager, and bring the whole surviving cluster to the new epoch.
//
// The assertion that carries the most weight is not "reform returned 200" — it
// is that the survivor becomes writable BECAUSE it holds the formation licence
// and no stream exists yet (formationWriter && !streamSeen), and then stops
// being a direct writer the moment the log appears. A reform that left the
// licence in place after stream creation would keep answering 200 to writes
// that never reach the log and get erased by the next snapshot, which no
// convergence check performed straight afterwards would notice.
func TestE2EForceReform(t *testing.T) {
	c := newE2ECluster(t, true, []e2eNodeSpec{
		{Name: "prime", Octet: 1, CosmosNode: 2, Lighthouse: true},
		{Name: "mantwo", Octet: 2, CosmosNode: 2},
		{Name: "manthree", Octet: 3, CosmosNode: 2},
		{Name: "agentone", Octet: 4, CosmosNode: 1, Agent: true},
	})

	for _, name := range []string{"prime", "mantwo", "manthree", "agentone"} {
		c.start(name)
		c.waitConnected(name, 240*time.Second)
	}
	c.waitFormed(420*time.Second, "prime", "mantwo", "manthree", "agentone")
	c.waitConverged(420*time.Second, "initial convergence", "prime", "mantwo", "manthree", "agentone")
	c.waitWritable(180*time.Second, "prime")

	// The R1->R3 scale-up is asserted here and nowhere else in the suite, and it is
	// also this scenario's precondition: until the log is genuinely on three peers,
	// killing two managers does not necessarily cost the survivor its quorum.
	c.waitStreamReplicas(420*time.Second, "prime", 3)

	before := c.oplogState("prime")
	if before.err != nil {
		t.Fatal("e2e: reading prime's op-log state:", before.err)
	}

	// the two-of-three loss that force-reform exists to escape
	c.kill("mantwo")
	c.kill("manthree")
	c.waitReadOnly(240*time.Second, "prime", "pre-reform")

	newEpoch := c.forceReform("prime")
	if newEpoch != before.epoch+1 {
		t.Fatalf("force-reform moved the epoch to %d, want %d (epoch is the publish fence; a reform that does not advance it fences nothing)",
			newEpoch, before.epoch+1)
	}

	// the survivor re-enters formation: new epoch, licence held, no log yet
	c.waitForDetail(180*time.Second, "prime re-enters formation at the new epoch", func() (bool, string) {
		st := c.oplogState("prime")
		return st.epoch == newEpoch && st.formationWriter && !st.streamSeen, st.String()
	})

	// and the licence is real: a write must actually land while no stream exists
	wrote := 0
	c.waitForDetail(240*time.Second, "prime writes directly in formation", func() (bool, string) {
		wrote++
		code, errW := c.tryWrite("prime", fmt.Sprintf("in-formation-%d", wrote))
		return errW == nil, fmt.Sprintf("attempt %d -> %q (%s)", wrote, code, c.oplogState("prime"))
	})

	// a replacement manager, enrolled BY the survivor, in formation, at the new
	// epoch — the normal enrollment flow is what the plan claims still works here
	c.enrollVia("prime", e2eNodeSpec{Name: "manfour", Octet: 5, CosmosNode: 2})
	c.start("manfour")
	c.waitConnected("manfour", 240*time.Second)

	// JetStream elects with two managers, the formation writer creates the log for
	// the new epoch, and formation ends by itself
	c.waitForDetail(420*time.Second, "log created at the new epoch and formation ends", func() (bool, string) {
		st := c.oplogState("prime")
		return st.epoch == newEpoch && st.attached && !st.formationWriter, st.String()
	})

	c.waitFormed(420*time.Second, "manfour")
	c.waitConverged(420*time.Second, "survivor and replacement converged at the new epoch", "prime", "manfour")

	// The agent was never blocked and never left. It has to end up on the new
	// epoch by snapshot; this is checked on its own rather than folded into the
	// convergence wait so that an agent stranded at the old epoch reports as
	// exactly that instead of as a generic timeout.
	c.waitForDetail(420*time.Second, "agent adopts the new epoch", func() (bool, string) {
		st := c.oplogState("agentone")
		return st.epoch == newEpoch && st.attached, "agentone: " + st.String()
	})
	c.waitConverged(420*time.Second, "whole surviving cluster converged", "prime", "manfour", "agentone")
	c.assertNotHalted("after reform", "prime", "manfour", "agentone")
}

// TestE2EStaleManagerFenced: revive a manager that was blocked and left behind
// by a reform. It must never be heard from again in the new epoch.
//
// "Never heard from" is asserted three ways, because each can hold while another
// fails: its writes must not reach the survivors, it must not sit attached to
// the new log with state nobody else agreed to, and its credentials must be
// refused by a survivor. The credential probe is the only one that tests
// revocation directly — a stale node dials its OWN server for its own client
// connection, so it will report itself connected no matter how thoroughly it has
// been fenced, and any assertion built on its self-reported health is vacuous.
func TestE2EStaleManagerFenced(t *testing.T) {
	c := newE2ECluster(t, true, []e2eNodeSpec{
		{Name: "prime", Octet: 1, CosmosNode: 2, Lighthouse: true},
		{Name: "mantwo", Octet: 2, CosmosNode: 2},
		{Name: "manthree", Octet: 3, CosmosNode: 2},
	})

	for _, name := range []string{"prime", "mantwo", "manthree"} {
		c.start(name)
		c.waitConnected(name, 240*time.Second)
	}
	c.waitFormed(420*time.Second, "prime", "mantwo", "manthree")
	c.waitConverged(420*time.Second, "initial convergence", "prime", "mantwo", "manthree")
	c.waitWritable(180*time.Second, "prime")
	c.waitStreamReplicas(420*time.Second, "prime", 3)

	staleKey := c.apiKeys["mantwo"]
	if staleKey == "" {
		t.Fatal("e2e: no recorded API key for mantwo; the credential probe below would be vacuous")
	}

	// Positive control, taken while the cluster is healthy: these credentials DO
	// open a connection to prime right now. Without this, a later refusal proves
	// nothing — it could just as easily mean the probe never worked.
	probe := func(from, targetIP, user, pwd string) (bool, string) {
		out, err := c.node(from).post("/probe-nats", map[string]string{
			"URL":      "nats://" + targetIP + ":4222",
			"User":     user,
			"Password": pwd,
		})
		if err != nil {
			return false, "probe failed: " + err.Error()
		}
		connected, _ := out["connected"].(bool)
		detail, _ := out["error"].(string)
		return connected, detail
	}
	if ok, detail := probe("manthree", "127.0.1.1", "mantwo", staleKey); !ok {
		t.Fatalf("e2e: mantwo's credentials were already refused by prime BEFORE any fencing (%s) — the revocation assertion below would pass for the wrong reason", detail)
	}

	before := c.oplogState("prime")
	if before.err != nil {
		t.Fatal("e2e: reading prime's op-log state:", before.err)
	}

	// lose quorum, reform around prime; mantwo is the manager we will revive
	c.kill("mantwo")
	c.kill("manthree")
	c.waitReadOnly(240*time.Second, "prime", "pre-reform")

	newEpoch := c.forceReform("prime")
	if newEpoch != before.epoch+1 {
		t.Fatalf("force-reform moved the epoch to %d, want %d", newEpoch, before.epoch+1)
	}

	// bring the new epoch to a working cluster before reviving the stale node, so
	// anything it disturbs is disturbing something that demonstrably worked
	c.enrollVia("prime", e2eNodeSpec{Name: "manfour", Octet: 5, CosmosNode: 2})
	c.start("manfour")
	c.waitConnected("manfour", 240*time.Second)
	c.waitForDetail(420*time.Second, "new epoch log is live", func() (bool, string) {
		st := c.oplogState("prime")
		return st.epoch == newEpoch && st.attached && !st.formationWriter, st.String()
	})
	c.waitFormed(420*time.Second, "manfour")
	c.waitConverged(420*time.Second, "new epoch converged before the revival", "prime", "manfour")

	healthyHash := c.dbHash("prime")
	if healthyHash == "" {
		t.Fatal("e2e: prime has no dump after reform")
	}

	// --- the revival
	c.start("mantwo")
	c.waitFor(120*time.Second, "revived stale manager answers its control API", func() bool {
		_, errS := c.node("mantwo").get("/status")
		return errS == nil
	})

	// 1. credentials. The device record was blocked by the reform and NATS was
	// restarted, so a survivor must refuse the stale identity.
	c.waitForDetail(240*time.Second, "prime refuses the stale manager's credentials", func() (bool, string) {
		ok, detail := probe("manfour", "127.0.1.1", "mantwo", staleKey)
		return !ok, fmt.Sprintf("connected=%v %s", ok, detail)
	})

	// 2. it must never land an op in the new epoch. Its own writes must fail, and
	// nothing it attempted may appear on the survivors.
	for i := 0; i < 6; i++ {
		name := fmt.Sprintf("stale-write-%d", i)
		if _, errW := c.node("mantwo").post("/create-device",
			map[string]string{"DeviceName": name, "IP": fmt.Sprintf("192.168.209.%d", 10+i)}); errW == nil {
			t.Errorf("stale manager's write SUCCEEDED (%s) — a fenced node accepted a config change", name)
		}
		time.Sleep(5 * time.Second)
	}
	for name := range c.deviceView("prime") {
		if len(name) >= 12 && name[:12] == "stale-write-" {
			t.Errorf("a stale manager's write reached prime as device %q — the epoch fence did not hold", name)
		}
	}

	// 3. it either adopted the new epoch or stayed isolated — the one state it
	// must never reach is attached to the new epoch while holding state the
	// cluster never agreed to.
	st := c.oplogState("mantwo")
	switch {
	case st.err != nil:
		t.Errorf("revived stale manager is unreachable: %v", st.err)
	case st.epoch == newEpoch:
		// it resynced: then it must actually agree with the cluster
		c.waitForDetail(300*time.Second, "resynced stale manager converges", func() (bool, string) {
			return c.dbHash("mantwo") == c.dbHash("prime"),
				fmt.Sprintf("mantwo=%.8s prime=%.8s (%s)", c.dbHash("mantwo"), c.dbHash("prime"), c.oplogState("mantwo"))
		})
	case st.epoch == before.epoch:
		if st.attached {
			t.Errorf("revived stale manager is ATTACHED at the abandoned epoch %d (%s) — it recreated the log it was fenced out of, which is a second live history",
				st.epoch, st)
		}
		if st.configWritable {
			t.Errorf("revived stale manager reports config writable at the abandoned epoch %d (%s)", st.epoch, st)
		}
		t.Logf("revived stale manager stayed isolated at epoch %d (%s)", st.epoch, st)
	default:
		t.Errorf("revived stale manager is at unexpected epoch %d, want %d (adopted) or %d (isolated): %s",
			st.epoch, newEpoch, before.epoch, st)
	}

	// 4. whatever the stale node did, the live cluster is undisturbed
	c.assertNotHalted("after the stale manager was revived", "prime", "manfour")
	c.waitConverged(300*time.Second, "live cluster still converged after the revival", "prime", "manfour")
}
