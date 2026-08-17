//go:build e2e

package constellation

// Availability and catch-up scenarios for the op-log: what a node does when the
// stream stops accepting writes, and what it does when it has been away longer
// than the log remembers. Both are the paths where a wrong answer is silent —
// a node that writes locally instead of refusing, or one that quietly resumes
// mid-log after entries it never saw, looks healthy and is permanently forked.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// a burst of writes outruns the 20s default client timeout; these calls are
// server-side loops, not the fast control endpoints the rest of the suite polls
var e2eHTTPLong = &http.Client{Timeout: 5 * time.Minute}

// burstUsers writes count users on the node in a single request and returns how
// many landed. User ops are the cheap op kind: no reaction, so no mesh restart.
func (c *e2eCluster) burstUsers(node, prefix string, count int) (int, error) {
	body := fmt.Sprintf(`{"Prefix":%q,"Count":%d}`, prefix, count)
	resp, err := e2eHTTPLong.Post(c.node(node).controlURL()+"/create-users",
		"application/json", strings.NewReader(body))
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	out := map[string]interface{}{}
	json.NewDecoder(resp.Body).Decode(&out)
	created, _ := out["created"].(float64)
	if resp.StatusCode != 200 {
		return int(created), fmt.Errorf("create-users on %s: HTTP %d after %v writes: %v",
			node, resp.StatusCode, out["created"], out["error"])
	}
	return int(created), nil
}

// oplogPos returns the node's (epoch, seq); -1 seq means the node did not answer.
func (c *e2eCluster) oplogPos(node string) (uint64, int64) {
	st, err := c.node(node).get("/oplog")
	if err != nil {
		return 0, -1
	}
	epoch, _ := st["epoch"].(float64)
	seq, _ := st["seq"].(float64)
	return uint64(epoch), int64(seq)
}

func (c *e2eCluster) assertNotHalted(what string, names ...string) {
	c.t.Helper()
	for _, name := range names {
		st, err := c.node(name).get("/oplog")
		if err != nil {
			c.t.Errorf("%s: /oplog on %s: %v", what, name, err)
			continue
		}
		if halted, _ := st["halted"].(bool); halted {
			c.t.Errorf("%s: %s apply loop HALTED: %v", what, name, st["haltReason"])
		}
	}
}

// tryWrite attempts one user write and reports the classified outcome: "" on
// success, otherwise the control API's error code ("read-only", "duplicate", …).
func (c *e2eCluster) tryWrite(node, nickname string) (code string, err error) {
	out, err := c.node(node).post("/create-user", map[string]string{"Nickname": nickname})
	if err == nil {
		return "", nil
	}
	code, _ = out["code"].(string)
	if code == "" {
		code = "<no code: " + err.Error() + ">"
	}
	return code, err
}

// streamLastSeq returns the op-log stream's last sequence as the node sees it,
// or -1 if the stream isn't reachable from there.
func (c *e2eCluster) streamLastSeq(node string) int64 {
	out, err := c.node(node).get("/stream-info")
	if err != nil {
		return -1
	}
	if exists, _ := out["exists"].(bool); !exists {
		return -1
	}
	last, _ := out["lastSeq"].(float64)
	return int64(last)
}

// TestE2EOplogDeadConsumerRecovery: a node whose consumer dies while its stream
// stays perfectly healthy must notice and rebuild it.
//
// This is the deterministic form of a failure that was first seen by luck. In a
// full-suite gate run, an HA manager that never went down lost its ordered
// consumer during a JetStream quorum outage and never got it back: it logged one
// "Attached to cosmos-oplog-e1 from seq 1" and nothing afterwards, while both
// revived peers reached seq 2. Its publishes were still ACKED, so every write
// returned 503 apply-timeout rather than a 409, it fell permanently behind, and
// it reported attached and writable throughout. That reproduced once in seven
// runs, which is no basis for coverage of a defect that leaves the surviving
// manager of an HA outage silently dead.
//
// So the test creates the state rather than the weather that sometimes causes
// it: /break-oplog-consumer unsubscribes the live subscription and leaves the
// attached latch set, which is what the outage left behind. The stream is never
// disturbed — same epoch, StreamInfo answering normally — which is precisely why
// the two original detach conditions (epoch moved, ErrStreamNotFound) are blind
// to it, and why the probe has to key on progress instead.
//
// The wedge is asserted BEFORE the recovery is, so the test cannot pass without
// having reached the defect: on code without the progress check the first half
// passes and the second half fails.
func TestE2EOplogDeadConsumerRecovery(t *testing.T) {
	c := newE2ECluster(t, false, []e2eNodeSpec{
		{Name: "prime", Octet: 1, CosmosNode: 2, Lighthouse: true},
		{Name: "agentone", Octet: 2, CosmosNode: 1, Agent: true},
	})

	c.start("prime")
	c.waitConnected("prime", 90*time.Second)
	c.start("agentone")
	c.waitConnected("agentone", 120*time.Second)
	c.waitFormed(300*time.Second, "prime", "agentone")
	c.waitConverged(180*time.Second, "initial convergence", "prime", "agentone")
	c.waitWritable(120*time.Second, "prime", "agentone")

	_, seqBefore := c.oplogPos("prime")

	out, err := c.node("prime").post("/break-oplog-consumer", map[string]interface{}{})
	if err != nil {
		t.Fatal("e2e: break prime's consumer:", err)
	}
	if broken, _ := out["broken"].(bool); !broken {
		t.Fatalf("e2e: prime had no live subscription to break (%v) — it was not attached, so this run would prove nothing", out)
	}
	if attached, _ := out["attached"].(bool); !attached {
		t.Fatal("e2e: breaking the subscription also cleared the attached latch; the wedged state was not created")
	}

	// --- half one: prove the wedge was real.
	//
	// Two independent pieces of evidence, and they are split deliberately.
	//
	// The STATE is established by the break response above: a live subscription
	// existed, it was torn down, and the attached latch survived. That is the
	// defect's shape and it is proven without reference to timing.
	//
	// The CONSEQUENCE is established here: the node's own write must not succeed.
	// A write that succeeds would mean the consumer was never really dead, which
	// is the one outcome that would make everything below vacuous — the same
	// vacuous-pass trap that let an earlier version of the snapshot scenario go
	// green while testing nothing.
	//
	// Both refusal codes are accepted, because both are correct and which one
	// appears is a race this test deliberately does not pin. While the node still
	// believes it is attached the publish is ACKED and never applied, giving 503
	// apply-timeout; once the stall detector has detached it, the node is
	// read-only and gives 409. Detection can be one probe (~6s, when the client's
	// async error handler arms the counter) or three (~18s, when it does not), and
	// this write is issued within milliseconds of the break, so ordinarily it is
	// the 503 — but requiring that would make the test flaky against the fast path
	// for no gain in rigour. A position-based guard ("attached and behind") was
	// tried and discarded: a node merely catching up looks identical.
	code, errW := c.tryWrite("prime", "wedged-write")
	if errW == nil {
		t.Fatal("e2e: prime's write SUCCEEDED immediately after its consumer was killed — the wedged state was never created, so this run proves nothing about recovery")
	}
	switch code {
	case "apply-timeout", "read-only":
		t.Logf("e2e: wedged write refused as %q (apply-timeout = still latched, read-only = stall already detected)", code)
	default:
		t.Fatalf("e2e: prime's write while wedged returned %q, want apply-timeout or read-only; anything else means the write failed for a reason unrelated to the dead consumer and the recovery assertion below would not be testing the reported defect", code)
	}

	// drive the log further forward from the OTHER node, so the gap prime has to
	// close is real work rather than its own single failed op
	if created, errB := c.burstUsers("agentone", "dead-consumer", 12); errB != nil {
		t.Fatalf("e2e: agent burst: %v (created %d)", errB, created)
	}

	// --- half two: it must dig itself out.
	// The probe runs every 6s and needs 3 consecutive stalled observations, so
	// recovery is expected in roughly 20s; the budget is generous because the
	// rebuild itself can lose a round to a busy server.
	c.waitForDetail(180*time.Second, "prime rebuilds its consumer and catches up",
		func() (bool, string) {
			st := c.oplogState("prime")
			last := c.streamLastSeq("agentone")
			return st.attached && !st.halted && last >= 0 && st.seq >= last,
				fmt.Sprintf("prime=%s streamLastSeq=%d", st, last)
		})

	if _, seqAfter := c.oplogPos("prime"); seqAfter <= seqBefore {
		t.Errorf("prime's sequence did not advance past %d (now %d) — it reattached without catching up", seqBefore, seqAfter)
	}

	// The symptom the user would actually report: writes answering 503 forever.
	// A node that has genuinely rebuilt its consumer applies its own op again.
	recovered := 0
	c.waitForDetail(120*time.Second, "prime's own writes work again", func() (bool, string) {
		recovered++
		code, errW := c.tryWrite("prime", fmt.Sprintf("post-wedge-%d", recovered))
		return errW == nil, fmt.Sprintf("attempt %d -> %q", recovered, code)
	})

	c.waitConverged(180*time.Second, "converged after the consumer rebuild", "prime", "agentone")
	c.assertNotHalted("after the consumer rebuild", "prime", "agentone")
}

// TestE2ENoQuorumReadOnly: an HA manager that loses quorum must refuse writes
// with 409 read-only and keep serving reads, then recover when quorum returns.
//
// The failure this guards against is not "the write errors" — it is a write that
// SUCCEEDS. oplogWriteMode's direct-write branches are all reachable on a live
// cluster member (a device reaction calls RestartNebula, which clears
// NebulaStarted and the device cache, so a real member briefly looks standalone),
// and a manager that answers 200 by writing to its own SQLite while cut off from
// the log has forked the cluster silently. So the assertion below is two-sided:
// the write must fail, and it must fail with the read-only classification rather
// than any error at all.
func TestE2ENoQuorumReadOnly(t *testing.T) {
	c := newE2ECluster(t, true, []e2eNodeSpec{
		{Name: "prime", Octet: 1, CosmosNode: 2, Lighthouse: true},
		{Name: "mantwo", Octet: 2, CosmosNode: 2},
		{Name: "manthree", Octet: 3, CosmosNode: 2},
	})

	for _, name := range []string{"prime", "mantwo", "manthree"} {
		c.start(name)
		c.waitConnected(name, 180*time.Second)
	}
	// Formation must be OVER before anything here means what it says: while the
	// formation writer still holds its licence its writes go straight to SQLite,
	// so they succeed without the log and the sequence never moves.
	c.waitFormed(420*time.Second, "prime", "mantwo", "manthree")
	c.waitConverged(300*time.Second, "initial convergence", "prime", "mantwo", "manthree")

	// The stream is created R1 and raised to R3 asynchronously. Killing two
	// managers before that lands would leave prime holding the only replica, its
	// writes would keep succeeding, and this test would fail while the product was
	// correct — so wait for the replication that makes "lose two of three" mean
	// quorum loss at all.
	c.waitStreamReplicas(420*time.Second, "prime", 3)

	// Baseline: with all three up this call must succeed, so a later failure is
	// attributable to quorum loss rather than to the endpoint never having worked.
	// Polled, not one-shot — an unrelated device reaction can have prime briefly
	// detached, and that 409 is correct behavior rather than the thing under test.
	base := 0
	c.waitForDetail(180*time.Second, "baseline write with full quorum", func() (bool, string) {
		base++
		code, errW := c.tryWrite("prime", fmt.Sprintf("quorum-baseline-%d", base))
		return errW == nil, fmt.Sprintf("attempt %d -> %q", base, code)
	})
	c.waitConverged(180*time.Second, "baseline write converged", "prime", "mantwo", "manthree")

	devicesBefore := len(c.deviceView("prime"))
	if devicesBefore == 0 {
		t.Fatal("e2e: prime reports no devices before the kill — the read-availability assertion below would be vacuous")
	}

	// two of three: the R3 stream has no quorum and cannot accept a publish
	c.kill("mantwo")
	c.kill("manthree")

	// Writes may still succeed for a moment while the cluster notices the loss, so
	// poll — but the outcome we are polling for is specific. An "internal" or a
	// bare transport error would mean the read-only contract collapsed into a
	// generic 500, which the UI shows as an unexplained failure instead of the
	// "config read-only" toast.
	attempt := 0
	seen := []string{}
	c.waitForDetail(180*time.Second, "prime refuses writes without quorum", func() (bool, string) {
		attempt++
		code, _ := c.tryWrite("prime", fmt.Sprintf("quorum-lost-%d", attempt))
		if code != "" {
			seen = append(seen, code)
		}
		return code == "read-only", fmt.Sprintf("attempt %d -> %q", attempt, code)
	})
	for _, code := range seen {
		switch code {
		case "read-only", "apply-timeout":
		default:
			t.Errorf("write without quorum was rejected as %q, want read-only (or apply-timeout during the transition)", code)
		}
	}

	// Baseline taken only once the node is settled into read-only. Sampling it
	// before the kill would be wrong: an attempt made while the cluster had not
	// yet noticed the loss can legitimately have succeeded and moved the log.
	_, seqReadOnly := c.oplogPos("prime")

	// Reads keep serving from local SQLite throughout, and every further write
	// stays refused. Sampled repeatedly rather than once, because a read path that
	// only breaks when the client's reconnect backoff kicks in, or a write path
	// that starts answering 200 once the node decides it looks standalone, would
	// both survive a single check.
	for i := 0; i < 5; i++ {
		if got := len(c.deviceView("prime")); got != devicesBefore {
			t.Errorf("read while read-only returned %d devices, want %d (reads must never depend on quorum)", got, devicesBefore)
		}
		attempt++
		if code, err := c.tryWrite("prime", fmt.Sprintf("quorum-lost-%d", attempt)); err == nil {
			t.Fatalf("write SUCCEEDED on a manager with no quorum (sample %d) — the write was applied outside the log and this node has forked", i)
		} else if code != "read-only" {
			t.Errorf("sustained write refusal returned %q, want read-only", code)
		}
		time.Sleep(3 * time.Second)
	}

	// nothing above is an apply-time fault, so the loop must still be alive
	c.assertNotHalted("during quorum loss", "prime")

	// refused writes leave no trace: the log cannot have advanced
	if _, seqNow := c.oplogPos("prime"); seqNow != seqReadOnly {
		t.Errorf("op-log sequence moved from %d to %d while every write was being refused", seqReadOnly, seqNow)
	}

	// recovery
	c.start("mantwo")
	c.start("manthree")
	c.waitConnected("mantwo", 180*time.Second)
	c.waitConnected("manthree", 180*time.Second)

	// This is the natural, racy path to the dead-consumer defect that
	// TestE2EOplogDeadConsumerRecovery now covers deterministically — it reproduced
	// here once in seven full-suite runs. It is kept as a second net over the real
	// outage rather than a synthetic one, so the detail below reports the state
	// that distinguishes the two ways this can fail: "read-only" means quorum never
	// came back, whereas "apply-timeout" with the stream ahead of prime's sequence
	// means quorum returned but prime's consumer did not, which is the defect.
	recovered := 0
	c.waitForDetail(300*time.Second, "writes recover once quorum returns", func() (bool, string) {
		recovered++
		code, err := c.tryWrite("prime", fmt.Sprintf("quorum-back-%d", recovered))
		return err == nil, fmt.Sprintf("attempt %d -> %q; prime=%s streamLastSeq=%d",
			recovered, code, c.oplogState("prime"), c.streamLastSeq("mantwo"))
	})
	c.waitConverged(300*time.Second, "converged after quorum recovery", "prime", "mantwo", "manthree")
	c.assertNotHalted("after quorum recovery", "prime", "mantwo", "manthree")
}

// TestE2ESnapshotFastForward: a node that was down while more than the log's
// 100-entry window went by cannot replay its way back, and must not try. It has
// to detect the gap, take a peer snapshot, and re-attach at the snapshot's
// sequence — with the retry loop covering retention moving again mid-install.
//
// Burst size is what makes this test real: 120 writes against MaxMsgs 100
// guarantees the returning node's next-needed sequence has been discarded, so
// the replay path is not merely unlikely, it is impossible. A smaller burst
// would let the node catch up by ordinary replay and the snapshot path would go
// untested while the test still passed.
func TestE2ESnapshotFastForward(t *testing.T) {
	const burst = 120

	// Agent variant: the agent reaches JetStream over its leafnode, so its
	// snapshot request and its re-attach both traverse a link the managers'
	// do not.
	t.Run("agent", func(t *testing.T) {
		c := newE2ECluster(t, false, []e2eNodeSpec{
			{Name: "prime", Octet: 1, CosmosNode: 2, Lighthouse: true},
			{Name: "agentone", Octet: 2, CosmosNode: 1, Agent: true},
		})

		c.start("prime")
		c.waitConnected("prime", 90*time.Second)
		c.start("agentone")
		c.waitConnected("agentone", 120*time.Second)
		// formation must be over, or prime's burst writes go direct to SQLite,
		// the log never advances, and the retention guard below correctly refuses
		// to call this a test of the snapshot path
		c.waitFormed(300*time.Second, "prime", "agentone")
		c.waitConverged(180*time.Second, "initial convergence", "prime", "agentone")
		c.waitWritable(120*time.Second, "prime")

		_, seqAtKill := c.oplogPos("agentone")
		if seqAtKill < 0 {
			t.Fatal("e2e: could not read agentone's op-log position before killing it")
		}
		c.kill("agentone")

		created, err := c.burstUsers("prime", "ff-agent", burst)
		if err != nil {
			t.Fatalf("e2e: burst on prime: %v (created %d)", err, created)
		}
		if created != burst {
			t.Fatalf("e2e: burst wrote %d of %d users; below the 100-entry retention the gap path is not forced", created, burst)
		}

		_, seqAfterBurst := c.oplogPos("prime")
		if seqAfterBurst < seqAtKill+int64(oplogMaxMsgs) {
			t.Fatalf("e2e: log advanced from %d to %d, less than the %d-entry window — the entry agentone needs may still be in the stream, so this run would not test the snapshot path",
				seqAtKill, seqAfterBurst, oplogMaxMsgs)
		}

		c.start("agentone")
		c.waitConnected("agentone", 180*time.Second)
		c.waitConverged(300*time.Second, "agent caught up past retention", "prime", "agentone")
		c.assertNotHalted("after agent fast-forward", "prime", "agentone")

		// converged at the burst's own sequence, not at some earlier point both
		// nodes happen to share
		_, seqAgent := c.oplogPos("agentone")
		if seqAgent < seqAfterBurst {
			t.Errorf("agentone converged at seq %d, behind the burst's %d", seqAgent, seqAfterBurst)
		}
	})

	// Manager variant, with writes continuing *through* the rejoin. This is the
	// case the mandatory gap re-check exists for: retention can move past a
	// snapshot between building it and installing it, and a node that skips the
	// re-check lands one entry short of the stream and stays there — attached,
	// unhalted, and permanently behind.
	t.Run("manager", func(t *testing.T) {
		c := newE2ECluster(t, true, []e2eNodeSpec{
			{Name: "prime", Octet: 1, CosmosNode: 2, Lighthouse: true},
			{Name: "mantwo", Octet: 2, CosmosNode: 2},
			{Name: "manthree", Octet: 3, CosmosNode: 2},
		})

		for _, name := range []string{"prime", "mantwo", "manthree"} {
			c.start(name)
			c.waitConnected(name, 180*time.Second)
		}
		c.waitFormed(420*time.Second, "prime", "mantwo", "manthree")
		c.waitConverged(300*time.Second, "initial convergence", "prime", "mantwo", "manthree")
		c.waitWritable(180*time.Second, "prime")
		// mantwo must actually hold a replica for its absence to be a real gap
		c.waitStreamReplicas(420*time.Second, "prime", 3)

		_, seqAtKill := c.oplogPos("mantwo")
		if seqAtKill < 0 {
			t.Fatal("e2e: could not read mantwo's op-log position before killing it")
		}
		// one of three: the surviving two keep quorum, so writes continue
		c.kill("mantwo")

		created, err := c.burstUsers("prime", "ff-mgr", burst)
		if err != nil {
			t.Fatalf("e2e: burst on prime: %v (created %d)", err, created)
		}
		if created != burst {
			t.Fatalf("e2e: burst wrote %d of %d users; below the 100-entry retention the gap path is not forced", created, burst)
		}

		_, seqAfterBurst := c.oplogPos("prime")
		if seqAfterBurst < seqAtKill+int64(oplogMaxMsgs) {
			t.Fatalf("e2e: log advanced from %d to %d, less than the %d-entry window — mantwo could still replay, so this run would not test the snapshot path",
				seqAtKill, seqAfterBurst, oplogMaxMsgs)
		}

		// keep the log moving while mantwo comes back, so its snapshot has a real
		// chance of ageing out during install
		churn := make(chan error, 1)
		go func() {
			_, err := c.burstUsers("prime", "ff-mgr-churn", burst)
			churn <- err
		}()

		c.start("mantwo")
		c.waitConnected("mantwo", 240*time.Second)

		if err := <-churn; err != nil {
			// a churn write may legitimately lose to a bounce window; it must not
			// die on anything else
			t.Logf("e2e: churn burst ended early: %v", err)
		}

		c.waitConverged(420*time.Second, "manager caught up past retention while the log moved", "prime", "mantwo", "manthree")
		c.assertNotHalted("after manager fast-forward", "prime", "mantwo", "manthree")

		_, seqMantwo := c.oplogPos("mantwo")
		if seqMantwo < seqAfterBurst {
			t.Errorf("mantwo converged at seq %d, behind the first burst's %d", seqMantwo, seqAfterBurst)
		}
	})
}
