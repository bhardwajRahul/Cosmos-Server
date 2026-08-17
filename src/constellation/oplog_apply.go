package constellation

import (
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/azukaar/cosmos-server/src/utils"
)

// The apply loop is the only thing that mutates replicated state on this node.
// It consumes the stream strictly in order and commits each entry's sequence in
// the same SQLite transaction as its data, which is what makes apply
// exactly-once regardless of JetStream redelivery.

var oplogAttached atomic.Bool
var oplogHalted atomic.Bool
var oplogHaltReason atomic.Value

// oplogStreamSeen: a log for the current epoch has been observed on this node.
// It ends formation's direct-write licence (see oplogWriteMode) at stream
// creation rather than at attach, so no write can slip past a log that already
// exists. Deliberately NOT cleared by StopOplogApply — a RestartNebula bounce
// must not reopen that window. Only a reform, which builds a new epoch, does.
var oplogStreamSeen atomic.Bool

// oplogAttachedEpoch: the epoch of the stream the current subscription belongs
// to, so the liveness probe can tell "my log is gone" from "my epoch moved".
var oplogAttachedEpoch atomic.Uint64

var oplogMu sync.Mutex
var oplogSub *nats.Subscription
var oplogStop chan struct{}
var oplogRunning bool

// serializes application so a redelivery can't race the entry behind it
var oplogApplyMu sync.Mutex

const oplogSupervisorTick = 2 * time.Second

// StartOplogApply brings up the apply loop; safe to call repeatedly.
func StartOplogApply() {
	oplogMu.Lock()
	if oplogRunning {
		oplogMu.Unlock()
		return
	}
	oplogRunning = true
	stop := make(chan struct{})
	oplogStop = stop
	oplogMu.Unlock()

	oplogHalted.Store(false)

	go oplogSupervisor(stop)
}

// StopOplogApply tears the loop down. Never blocks: it runs from
// CloseNATSClient/StopNATS, which RestartNebula calls from an apply reaction.
func StopOplogApply() {
	oplogMu.Lock()
	if !oplogRunning {
		oplogMu.Unlock()
		return
	}
	oplogRunning = false
	stop := oplogStop
	oplogStop = nil
	sub := oplogSub
	oplogSub = nil
	oplogMu.Unlock()

	oplogAttached.Store(false)
	if stop != nil {
		close(stop)
	}
	if sub != nil {
		sub.Unsubscribe()
	}
}

// oplogSupervisor keeps the subscription attached: a node with no reachable
// stream idles here and catches up (by snapshot if it fell off retention) as
// soon as one appears.
func oplogSupervisor(stop chan struct{}) {
	waitingSince := time.Now()

	for {
		select {
		case <-stop:
			return
		default:
		}

		if !oplogAttached.Load() && !oplogHalted.Load() {
			if err := oplogAttach(waitingSince); err != nil {
				utils.Debug("[OPLOG] not attached yet: " + err.Error())
			}
		} else if oplogAttached.Load() {
			// being attached is not evidence of being attached to anything
			oplogCheckAttachedStream()
			// doubles as the scale-up retry loop: placing a 3-way replica group
			// fails until the joining manager's cluster route is really up
			oplogMaintainReplicas()
		}

		select {
		case <-stop:
			return
		case <-time.After(oplogSupervisorTick):
		}
	}
}

// oplogStreamCheckEvery paces the liveness probe below. The supervisor ticks
// every 2s, which is more often than a StreamInfo round-trip needs to run.
const oplogStreamCheckEvery = 6 * time.Second

// oplogStallProbes: how many consecutive probes may see the log ahead of us with
// our own position frozen before we conclude the consumer is dead. Three at the
// pacing above is ~18s of provable non-progress.
const oplogStallProbes = 3

var oplogStreamCheckMu sync.Mutex
var oplogStreamCheckedAt time.Time
var oplogStallCount int
var oplogStallLastApplied uint64

// oplogCheckAttachedStream detaches when the log this node is attached to is
// gone, or when its epoch has moved on underneath it.
//
// oplogAttached is a latch: it is set once at subscribe time and cleared only by
// an explicit detach. A JetStream subscription whose stream is DELETED does not
// error — it simply goes quiet — so without this probe the latch stays set
// forever, the supervisor keeps taking the attached branch, and oplogAttach is
// never called again. Every recovery path lives inside oplogAttach, including
// oplogAdoptFromPeer, so a node that was healthy at the moment of a force-reform
// was the one node that could never learn about the new epoch. Worse, it kept
// reporting itself writable (oplogWriteMode returns publish whenever the latch is
// set) while every publish would have failed against a stream that no longer
// exists: wedged, and looking perfectly healthy while wedged.
func oplogCheckAttachedStream() {
	oplogStreamCheckMu.Lock()
	if time.Since(oplogStreamCheckedAt) < oplogStreamCheckEvery {
		oplogStreamCheckMu.Unlock()
		return
	}
	oplogStreamCheckedAt = time.Now()
	oplogStreamCheckMu.Unlock()

	// a reform on THIS node moves the epoch without touching the subscription
	epoch := utils.GetOplogEpoch()
	if oplogAttachedEpoch.Load() != epoch {
		utils.Log("[OPLOG] Local epoch moved to " + strconv.FormatUint(epoch, 10) + ", re-attaching")
		oplogDetach()
		return
	}

	clientConfigLock.RLock()
	jsCtx := js
	clientConfigLock.RUnlock()
	if jsCtx == nil {
		return
	}

	si, err := jsCtx.StreamInfo(oplogStreamName(epoch))

	// A definitive "it is not there" detaches. Any other error is UNOBSERVABLE
	// rather than benign: during a quorum outage StreamInfo simply doesn't answer,
	// so we learn nothing and must change nothing — including the stall counter
	// below, which has to survive the outage to be useful in the recovery window
	// right after it.
	if errors.Is(err, nats.ErrStreamNotFound) {
		utils.Warn("[OPLOG] " + oplogStreamName(epoch) + " no longer exists; detaching to find the current epoch")
		oplogDetach()
		return
	}
	if err != nil {
		return
	}

	if oplogNoteApplyProgress(si.State.LastSeq) {
		utils.Warn("[OPLOG] attached to " + oplogStreamName(epoch) + " but stuck at seq " +
			strconv.FormatUint(utils.GetLastAppliedSeq(), 10) + " while the log is at " +
			strconv.FormatUint(si.State.LastSeq, 10) + "; rebuilding the consumer")
		oplogDetach()
	}
}

// oplogNoteApplyProgress reports whether the subscription has provably stopped
// consuming: the log is ahead of us and our own position has not moved across
// oplogStallProbes probes.
//
// This keys on PROGRESS rather than on error classification, and that is the
// whole point. A consumer can die without producing an error anywhere — an
// ordered consumer lost during a quorum outage leaves the stream healthy, the
// epoch unchanged, and StreamInfo answering normally, so both of the checks above
// pass while nothing is ever delivered again. The node then publishes into a log
// it will never read back: the ack succeeds, no waiter is ever notified, and every
// write returns 503 apply-timeout forever while the node reports itself attached
// and writable, falling further behind for as long as it runs.
//
// Detaching restores the truthful answer too. A detached node is read-only, so
// the same outage returns 409 "config read-only" — which is what the plan
// specifies for lost quorum — instead of a 503 that claims the write might yet
// land. It also stops NATSStatus reporting ConfigWritable on a node that cannot
// apply anything, which was the UI telling an operator a wedged node was healthy.
//
// TIMING, because "3 probes × 6s" invites the wrong reading: the ~18s is measured
// from when JetStream starts ANSWERING again, not from the fault. The counter can
// only accumulate on a successful StreamInfo, so during the outage itself nothing
// is observed and nothing is counted — which is deliberate (see the caller) and is
// what makes the evidence land in the recovery window where the damage shows.
//
// Safe against the blips the caller deliberately ignores: any real delivery moves
// last_applied_seq and resets the count, so a healthy consumer working through a
// backlog, or one merely lagging a busy log between probes, never trips it. That
// includes a run of REJECTED entries — a constraint violation still commits its
// sequence (see oplogApplyEnvelope), which is what we want here: the test is
// whether the consumer is alive, not whether its entries were accepted.
//
// One known false positive, left deliberately: GetLastAppliedSeq returns 0 when
// the store cannot be read, which looks frozen and would cycle detach/re-attach.
// A node whose SQLite reads are failing is already broken in a much larger way,
// and everything else in this file fails closed on the same condition — so this
// is documented rather than special-cased, to save the next reader diagnosing a
// reattach loop that is really a disk problem.
func oplogNoteApplyProgress(streamLast uint64) bool {
	applied := utils.GetLastAppliedSeq()

	oplogStreamCheckMu.Lock()
	defer oplogStreamCheckMu.Unlock()

	// caught up, or we moved since the last probe: the consumer is alive
	if streamLast <= applied || applied != oplogStallLastApplied {
		oplogStallLastApplied = applied
		oplogStallCount = 0
		return false
	}

	oplogStallCount++
	if oplogStallCount < oplogStallProbes {
		return false
	}
	oplogStallCount = 0
	return true
}

// oplogOwnsSubscription reports whether sub is the apply loop's current
// subscription, so an async error about someone else's doesn't arm anything.
//
// The pointer comparison relies on nats.go MUTATING the Subscription during its
// own recovery (resetOrderedConsumer calls applyNewSID on the same object)
// rather than allocating a replacement. That happens to hold in the exact
// failure this exists for, but it is a property of the client, not a contract.
// If a future version allocates instead, this stops matching and we lose the
// acceleration only — the progress probe still detaches, just at ~18s instead of
// ~6s. That degradation is silent by design: nothing fails and no test goes red.
// So do NOT "harden" this into something load-bearing; its whole value is that
// losing it costs speed and never correctness.
func oplogOwnsSubscription(sub *nats.Subscription) bool {
	oplogMu.Lock()
	defer oplogMu.Unlock()
	return oplogSub != nil && oplogSub == sub
}

// oplogArmStallDetector brings the counter to one short of the threshold, so the
// NEXT probe can act rather than waiting out the full window.
//
// Deliberately not a detach. nats.go knows a consumer is inactive long before
// three 6s probes can prove it, and using that shortens recovery from ~18s to
// ~6s — but the signal is a hint, not evidence. The probe still has to observe
// the log ahead of us with our position frozen, so a spurious or transient
// report costs nothing at all.
func oplogArmStallDetector() {
	oplogStreamCheckMu.Lock()
	if oplogStallCount < oplogStallProbes-1 {
		oplogStallCount = oplogStallProbes - 1
	}
	oplogStreamCheckMu.Unlock()
}

// oplogResetStallTracking clears the progress window, so a fresh subscription is
// never judged on the dead one's history.
func oplogResetStallTracking() {
	oplogStreamCheckMu.Lock()
	oplogStallCount = 0
	oplogStallLastApplied = utils.GetLastAppliedSeq()
	oplogStreamCheckMu.Unlock()
}

func oplogAttach(waitingSince time.Time) error {
	if err := ClientConnectToJS(); err != nil {
		return err
	}

	clientConfigLock.RLock()
	jsCtx := js
	clientConfigLock.RUnlock()

	if jsCtx == nil {
		return errors.New("oplog: no JetStream context")
	}

	epoch := utils.GetOplogEpoch()

	si, errInfo := jsCtx.StreamInfo(oplogStreamName(epoch))
	created := false
	if errInfo != nil {
		// No log at OUR epoch. That is not only the founder's situation — it is
		// equally what a node left behind by a force-reform sees (the reform renames
		// the stream, so its own epoch's name resolves to nothing) and what a
		// replacement manager enrolling at the default epoch 1 sees on a cluster that
		// has moved past it.
		//
		// So ask the cluster before creating anything. A snapshot reply is the only
		// message that carries the authoritative epoch, and creating here instead
		// would be actively harmful in both of those cases: it resurrects a stream
		// for an abandoned epoch, and the publish fence's entire premise is that
		// `...oplog.e<abandoned>.>` matches no stream. Previously this path returned
		// early, above every snapshot call site, so nothing ever triggered adoption
		// and a stale node either looped forever (agent) or seeded the dead epoch
		// (manager).
		if oplogAdoptFromPeer() {
			return errors.New("oplog: adopted a peer's epoch, re-attaching")
		}

		var errCreate error
		si, created, errCreate = createOplogStream(jsCtx, epoch, waitingSince)
		if errCreate != nil {
			return errCreate
		}
	}

	// formation is over for this node the moment a log exists, attached or not
	oplogStreamSeen.Store(true)

	// The gap path detaches asynchronously, so a message can still be in flight
	// while we catch up. Holding the apply lock makes it wait and then re-gate
	// against the post-snapshot position instead of writing over it.
	oplogApplyMu.Lock()
	defer oplogApplyMu.Unlock()

	last := utils.GetLastAppliedSeq()

	switch {
	// We just created this log, so it is empty and no peer can be serving it —
	// oplogSnapshotRouter only answers once attached, and nobody can be attached to
	// a stream that did not exist a moment ago. Our own store is the seed.
	//
	// Recording it HERE, before the subscribe below, is what closes the restart
	// window: until this is persisted, a node that died after creating the stream
	// would come back with its formation licence still set, oplogStreamSeen false
	// again (it is in-memory), and therefore a direct-write path into a log that
	// already exists. It also skips a fast-forward that is provably useless, which
	// used to stall every formation exit for the full 15s snapshot timeout.
	case created:
		if err := utils.MarkOplogBootstrapped(epoch); err != nil {
			return err
		}

	// the entry we need has aged out of the 100-message window
	case si.State.Msgs > 0 && si.State.FirstSeq > last+1:
		utils.Warn("[OPLOG] behind retention (need " + strconv.FormatUint(last+1, 10) +
			", stream starts at " + strconv.FormatUint(si.State.FirstSeq, 10) + "), fast-forwarding")
		if err := oplogFastForward(jsCtx, epoch); err != nil {
			return err
		}
		last = utils.GetLastAppliedSeq()

	// A node that has never materialized from this log cannot rebuild state by
	// replaying a log of deltas, however short it looks — the entries that created
	// the existing users, devices and CA are simply not in it. It has to snapshot.
	// If no peer answers, this node IS the seed and its own SQLite is the truth.
	//
	// Gated on the bootstrapped marker rather than on seq == 0: a founder seeds at
	// seq 0 and serves snapshots stamped seq 0, so a joiner that installs one is
	// still at seq 0 afterwards. Keyed on the sequence, this branch re-fired on
	// every attempt and the joiner never reached the Subscribe below.
	case !utils.IsOplogBootstrapped():
		if err := oplogFastForward(jsCtx, epoch); err != nil {
			// Only a node that can prove it holds the seed truth may answer an
			// unreachable peer by declaring its own store authoritative. Creating the
			// log is handled above, so what remains here is the formation writer
			// re-attaching to a stream it created in an earlier process. Anyone else
			// is a joiner whose peer is merely slow, and self-seeding there forks the
			// constellation into two histories that both look valid.
			if !utils.IsFormationWriter() {
				return err
			}
			utils.Warn("[OPLOG] no peer snapshot available, seeding the log from local state: " + err.Error())
			// founder: our own store is the seed truth for this epoch
			if errMark := utils.MarkOplogBootstrapped(epoch); errMark != nil {
				return errMark
			}
		}
		last = utils.GetLastAppliedSeq()
	}

	// fast-forward may have adopted a newer epoch, which retargets the stream
	if utils.GetOplogEpoch() != epoch {
		return errors.New("oplog: epoch changed while catching up, re-attaching")
	}

	sub, err := jsCtx.Subscribe(oplogSubjectWildcard(epoch), oplogHandleMsg,
		nats.OrderedConsumer(), nats.StartSequence(last+1))
	if err != nil {
		return err
	}

	oplogMu.Lock()
	// a Stop raced us in: drop the subscription instead of running detached
	if !oplogRunning {
		oplogMu.Unlock()
		sub.Unsubscribe()
		return errors.New("oplog: stopped while attaching")
	}
	oplogSub = sub
	oplogMu.Unlock()

	oplogResetStallTracking()
	oplogAttachedEpoch.Store(epoch)
	oplogAttached.Store(true)
	utils.Log("[OPLOG] Attached to " + oplogStreamName(epoch) + " from seq " + strconv.FormatUint(last+1, 10))

	// Formation ends here: the log exists and this node is applying it, so every
	// write from now on publishes like any other node's.
	//
	// Clearing LATE — at attach rather than at creation — is load-bearing, and the
	// self-seed gate above depends on it. If the writer creates the stream and the
	// process dies before attaching, the next start sees created == false (the
	// stream now exists) but still holds the licence, so it is still allowed to seed
	// itself. Move this to creation and that restart wedges forever: the one node
	// that IS the seed would sit waiting for a peer snapshot that nobody can serve.
	if utils.IsFormationWriter() {
		if err := utils.ClearFormationWriter(); err != nil {
			utils.Error("[OPLOG] Failed to end formation mode", err)
		} else {
			utils.Log("[OPLOG] Formation complete, this node now publishes to " + oplogStreamName(epoch))
		}
	}
	return nil
}

func oplogDetach() {
	oplogMu.Lock()
	sub := oplogSub
	oplogSub = nil
	oplogMu.Unlock()

	oplogAttached.Store(false)
	if sub != nil {
		sub.Unsubscribe()
	}
}

// oplogHalt stops applying rather than skipping an entry we don't understand:
// skipping would silently fork this node's state from the rest of the cluster.
func oplogHalt(err error, seq uint64) {
	oplogHalted.Store(true)
	oplogHaltReason.Store("seq " + strconv.FormatUint(seq, 10) + ": " + err.Error())
	utils.MajorError("[OPLOG] Apply loop halted at seq "+strconv.FormatUint(seq, 10)+
		", config replication has stopped on this node", err)
	go oplogDetach()
}

type seqAction int

const (
	seqApply seqAction = iota
	seqSkip
	seqGap
)

// oplogSeqAction gates delivery on strict contiguity: anything already applied
// is dropped, and anything past the next entry means the ones in between aged
// out of the log, which only a snapshot can repair.
func oplogSeqAction(seq uint64, last uint64) seqAction {
	switch {
	case seq <= last:
		return seqSkip
	case seq > last+1:
		return seqGap
	}
	return seqApply
}

func oplogHandleMsg(m *nats.Msg) {
	meta, err := m.Metadata()
	if err != nil {
		utils.Error("[OPLOG] message carries no JetStream metadata", err)
		return
	}
	seq := meta.Sequence.Stream

	oplogApplyMu.Lock()
	defer oplogApplyMu.Unlock()

	if oplogHalted.Load() {
		return
	}

	last := utils.GetLastAppliedSeq()

	switch oplogSeqAction(seq, last) {
	case seqSkip:
		// redelivery of something already committed — idempotent no-op
		utils.Debug("[OPLOG] skipping already-applied seq " + strconv.FormatUint(seq, 10))
		return
	case seqGap:
		utils.Warn("[OPLOG] sequence gap at " + strconv.FormatUint(seq, 10) +
			" (last applied " + strconv.FormatUint(last, 10) + "), fast-forwarding")
		oplogAttached.Store(false)
		go oplogDetach()
		return
	}

	var env OpEnvelope
	if err := json.Unmarshal(m.Data, &env); err != nil {
		oplogHalt(err, seq)
		return
	}

	if env.Epoch != utils.GetOplogEpoch() {
		// can't happen while the epoch is in the subject; consume it so the loop
		// stays contiguous rather than wedging on a forged entry
		utils.Warn("[OPLOG] epoch mismatch at seq " + strconv.FormatUint(seq, 10) + ", skipping entry")
		if err := utils.CommitOplogSeq(seq); err != nil {
			oplogHalt(err, seq)
		}
		return
	}

	if err := oplogApplyEnvelope(env, seq); err != nil {
		oplogHalt(err, seq)
	}
}

func oplogApplyEnvelope(env OpEnvelope, seq uint64) error {
	if env.V != oplogEnvVersion {
		return errors.New("oplog: unsupported envelope version " + strconv.Itoa(env.V))
	}

	// the waiter registry is what identifies our own ops: ReqID is unique, so
	// notifying an id we never registered is a no-op. Origin stays informational,
	// which keeps waiters working even when this node can't read its own name.
	if env.Kind == "domain" {
		return oplogApplyDomainEnvelope(env, seq)
	}
	if env.Kind != "db" {
		return errors.New("oplog: unknown op kind " + env.Kind)
	}

	doc, err := utils.DecodeOpDoc(env.Table, env.Op, env.Doc)
	if err != nil {
		return err
	}

	m := utils.Mutation{Table: env.Table, Op: env.Op, Filter: env.Filter, Doc: doc}

	var pre []utils.ConstellationDevice
	err = utils.ApplyOpTx(m, seq, &pre)

	var ec *utils.ErrConstraint
	if errors.As(err, &ec) {
		// A uniqueness rejection is a first-class outcome, not a fault: every replica
		// rejects it identically, so the sequence is still consumed and only the
		// originator learns it lost. (This is why mapSQLError must recognise BOTH
		// UNIQUE and PRIMARY KEY — users are keyed by nickname, so a duplicate user
		// arrives as a PK violation, and an unrecognised error here halts every node.)
		//
		// Deliberately two transactions rather than a SAVEPOINT: applyOpTx has already
		// rolled the data back, and we commit the sequence separately below. The crash
		// window in between is benign — on restart the node re-attaches at this same
		// sequence, hits the identical violation (the conflicting row is still there),
		// and commits. A replay re-rejects to the same end state; the only thing lost
		// is a waiter notification whose HTTP request died with the process.
		utils.Warn("[OPLOG] entry at seq " + strconv.FormatUint(seq, 10) + " rejected: " + err.Error())
		if errSeq := utils.CommitOplogSeq(seq); errSeq != nil {
			return errSeq
		}
		notifyOpWaiter(env.ReqID, err)
		return nil
	}
	if err != nil {
		return err
	}

	notifyOpWaiter(env.ReqID, nil)
	oplogReactToTableOp(m, pre)
	return nil
}

func oplogApplyDomainEnvelope(env OpEnvelope, seq uint64) error {
	d, ok := oplogDomains[env.Domain]
	if !ok {
		return errors.New("oplog: unknown domain " + env.Domain)
	}

	old, _ := d.Snapshot()

	utils.ConfigLock.Lock()
	err := d.Apply(env.Doc)
	utils.ConfigLock.Unlock()

	if err != nil {
		return err
	}

	// Apply MUST precede the sequence commit, and this ordering is load-bearing:
	// no transaction spans config.json and SQLite, so a crash between them has to
	// land on the safe side. Applied-but-not-committed replays the same full state,
	// which is harmless because domain payloads are idempotent "set" ops. Committed-
	// but-not-applied would drop the change permanently with nothing to detect it.
	// Do not hoist this above d.Apply.
	if err := utils.CommitOplogSeq(seq); err != nil {
		return err
	}

	notifyOpWaiter(env.ReqID, nil)
	if d.React != nil {
		d.React(old, env.Doc)
	}
	return nil
}

// deviceTopologyFields are the columns that reshape the nebula mesh or the NATS
// credential set; anything else is cosmetic to the network.
var deviceTopologyFields = map[string]bool{
	"DeviceName":     true,
	"IP":             true,
	"Blocked":        true,
	"CosmosNode":     true,
	"IsLighthouse":   true,
	"IsRelay":        true,
	"IsExitNode":     true,
	"IsLoadBalancer": true,
	"PublicHostname": true,
	"Port":           true,
	"PublicKey":      true,
	"APIKey":         true,
	"Fingerprint":    true,
}

// oplogReactToTableOp bounces only what the change actually invalidates: user
// rows need nothing, a tag rename needs the device cache, and only a real
// topology move is worth a nebula restart.
func oplogReactToTableOp(m utils.Mutation, pre []utils.ConstellationDevice) {
	if m.Table != "devices" {
		return
	}

	switch m.Op {
	case "insert", "insertMany", "delete", "deleteMany":
		go RestartNebula()
		return
	}

	fields, _ := m.Doc.(map[string]interface{})
	if utils.DeviceFieldsChanged(pre, fields, deviceTopologyFields) {
		go RestartNebula()
		return
	}

	go refreshDeviceCache()
}
