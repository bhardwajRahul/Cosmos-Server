package constellation

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/azukaar/cosmos-server/src/utils"
)

// A node that has been down longer than the log's 100-entry window can't replay
// its way back. It asks any healthy peer for a snapshot instead: the same
// mechanism for managers and agents, and the only way a brand-new node gets its
// initial state.

const oplogSnapshotSubject = "cosmos._global_.oplog.snapshot-request"
const oplogSnapshotTimeout = 15 * time.Second
const oplogFastForwardAttempts = 5

type OplogSnapshot struct {
	Epoch   uint64                     `json:"epoch"`
	Seq     uint64                     `json:"seq"`
	Dump    json.RawMessage            `json:"dump"`
	Domains map[string]json.RawMessage `json:"domains"`
}

// oplogSnapshotRequest carries the asker's epoch so the exchange is fenced on
// BOTH sides, not just at the requester.
type oplogSnapshotRequest struct {
	Epoch uint64 `json:"epoch"`
}

// oplogMayServeSnapshot reports whether this node is entitled to hand its state
// to a peer at all. Extracted from the router so the policy is testable on its
// own; the per-request epoch check stays with the request.
func oplogMayServeSnapshot() bool {
	if oplogHalted.Load() {
		return false
	}

	// A node the constellation has REMOVED must not be a source of truth for
	// anyone, and this needs its own check rather than falling out of the attached
	// gate below: self-fencing deliberately leaves a blocked node attached so it
	// can go on materializing as a read-only member, which means it would
	// otherwise sail straight past that gate and answer every joiner.
	//
	// This matters more than the write fence it completes. A snapshot is not just
	// device rows — snapshotDomains() walks every registered domain, so the payload
	// carries file:ca.key (the constellation CA private key), the auth_keys signing
	// keypair, api_tokens, openid_clients with their secrets, and file:rclone.conf
	// credentials. Answering here would let a removed machine hand the cluster's
	// entire secret material to every node that enrolls or falls off retention
	// afterwards.
	if isSelfBlocked() {
		return false
	}

	return oplogAttached.Load() || utils.IsFormationWriter()
}

// oplogSnapshotRouter answers snapshot requests from peers.
func oplogSnapshotRouter(conn *nats.Conn) {
	conn.Subscribe(oplogSnapshotSubject, func(m *nats.Msg) {
		// Only a node whose own loop is healthy and current can hand out state —
		// this is also what stops a mid-bootstrap node (attached to nothing yet)
		// from answering with the empty store it is about to fill.
		//
		// The formation writer is the one exception, and it has to be: during HA
		// formation there is no log to attach to, yet its store IS the seed truth
		// every enrolling peer needs.
		//
		// INVARIANT, and it lives in three files — do not weaken any of them
		// separately. Serving while unattached is safe ONLY because no peer can
		// reach this path until the stream exists: a non-licensed node is refused by
		// oplogMayCreateStream (oplog.go) while no stream exists, and the moment one
		// does exist oplogStreamSeen is set (oplog_apply.go), which is the same
		// moment the writer's direct-write licence stops applying. So by the time
		// anyone can ask, the writer's store is frozen and the snapshot is
		// consistent. If a peer could snapshot mid-formation it would install state
		// at (epoch, 0) while the writer kept making direct writes that appear in no
		// log and in no snapshot, and nothing would ever reconcile the two.
		if !oplogMayServeSnapshot() {
			return
		}

		// Epoch fence, responder side. A node behind the asker must stay SILENT
		// rather than answer with state from an epoch the asker has already left.
		// The asker would reject it — but core request/reply takes the first reply
		// and gives up, so one fast stale responder is enough to starve the current
		// peers and leave the asker believing nobody can help. Its next move is to
		// create a stream for its own epoch, which is exactly how an abandoned
		// epoch's log gets resurrected on a live cluster.
		//
		// A body we can't read, or a legacy one, decodes to epoch 0 and is served
		// as before.
		var req oplogSnapshotRequest
		json.Unmarshal(m.Data, &req)
		if req.Epoch > utils.GetOplogEpoch() {
			utils.Debug("[OPLOG] declining a snapshot request from epoch " +
				strconv.FormatUint(req.Epoch, 10) + ": this node is behind it")
			return
		}

		snap, err := buildOplogSnapshot()
		if err != nil {
			utils.Error("[OPLOG] Failed to build snapshot for a peer", err)
			return
		}

		// Refuse to serve an empty store. Install is replace-all, so answering
		// here would wipe the requester's good state. Guarded on both sides: the
		// requester cannot tell an empty peer from a legitimately empty cluster,
		// and this node can.
		if dumpIsEmpty(snap.Dump) {
			utils.Warn("[OPLOG] declining to serve a snapshot: this node's store is empty")
			return
		}

		payload, err := json.Marshal(snap)
		if err != nil {
			utils.Error("[OPLOG] Failed to marshal snapshot for a peer", err)
			return
		}
		m.Respond(payload)
	})
}

func buildOplogSnapshot() (OplogSnapshot, error) {
	// rows and the log position they belong to come out of one read tx
	dump, epoch, seq, err := utils.BuildSnapshot()
	if err != nil {
		return OplogSnapshot{}, err
	}

	// domain state lives in config.json and loose files rather than SQLite, so
	// the config lock is the closest thing to that tx we have
	utils.ConfigLock.Lock()
	domains := snapshotDomains()
	utils.ConfigLock.Unlock()

	return OplogSnapshot{Epoch: epoch, Seq: seq, Dump: dump, Domains: domains}, nil
}

// dumpIsEmpty reports whether a snapshot body carries no rows at all.
//
// A length check does NOT answer this and is actively misleading: an empty store
// marshals to `{"devices":[],"users":[]}` — 25 bytes, not zero — so `len(dump) == 0`
// only catches a missing field and would wave through a semantically empty
// snapshot straight into a replace-all install.
func dumpIsEmpty(dump []byte) bool {
	if len(dump) == 0 {
		return true
	}
	var d struct {
		Users   []json.RawMessage `json:"users"`
		Devices []json.RawMessage `json:"devices"`
	}
	if err := json.Unmarshal(dump, &d); err != nil {
		return true
	}
	return len(d.Users) == 0 && len(d.Devices) == 0
}

// oplogAdoptFromPeer asks the cluster for state when this node's own epoch has
// no log behind it, and installs whatever epoch comes back. Reports whether it
// adopted anything — if it did, the caller must re-attach, because the epoch and
// therefore the stream name have changed under it.
//
// This is the reform recovery path. It deliberately runs BEFORE stream creation:
// a node whose epoch has been left behind cannot tell "I am the founder" from "I
// have been reformed out" by looking at anything local, and only a peer can
// settle it.
func oplogAdoptFromPeer() bool {
	snap, err := requestOplogSnapshot()
	if err != nil {
		utils.Debug("[OPLOG] no peer answered a snapshot request: " + err.Error())
		return false
	}
	// see dumpIsEmpty: install is replace-all, so an empty reply would wipe us
	if dumpIsEmpty(snap.Dump) {
		return false
	}

	local := utils.GetOplogEpoch()
	if snap.Epoch < local {
		utils.Warn("[OPLOG] ignoring a snapshot from stale epoch " + strconv.FormatUint(snap.Epoch, 10))
		return false
	}
	// Same epoch and we already hold its state: there is nothing to learn, and
	// re-installing on every supervisor tick while we wait for the founder to
	// create the stream would be a busy loop over the whole store.
	if snap.Epoch == local && utils.IsOplogBootstrapped() {
		return false
	}

	before, _ := utils.BuildLogicalDump()
	if err := installOplogSnapshot(snap); err != nil {
		utils.Error("[OPLOG] Failed to install a peer's snapshot", err)
		return false
	}
	utils.Log("[OPLOG] Installed a peer snapshot at epoch " + strconv.FormatUint(snap.Epoch, 10) +
		" seq " + strconv.FormatUint(snap.Seq, 10))

	// moving epochs means the JetStream state we hold belongs to a cluster that
	// no longer exists; oplogDropStaleJetStream takes the restart over when so
	if snap.Epoch != local && oplogDropStaleJetStream() {
		return true
	}
	afterInstallReaction(before)
	return true
}

// oplogFastForward replaces local state wholesale from a peer's snapshot and
// loops until the stream's retention window actually covers where it landed.
func oplogFastForward(jsCtx nats.JetStreamContext, epoch uint64) error {
	for attempt := 1; attempt <= oplogFastForwardAttempts; attempt++ {
		snap, err := requestOplogSnapshot()
		if err != nil {
			return err
		}
		// see dumpIsEmpty: a byte-length check here would accept `{"devices":[],"users":[]}`
		if dumpIsEmpty(snap.Dump) {
			return errors.New("oplog: peer returned an empty snapshot, refusing to replace local state")
		}

		// a snapshot from an older epoch is a stale peer talking; try again
		if snap.Epoch < utils.GetOplogEpoch() {
			return errors.New("oplog: rejected snapshot from stale epoch " + strconv.FormatUint(snap.Epoch, 10))
		}

		before, _ := utils.BuildLogicalDump()

		if err := installOplogSnapshot(snap); err != nil {
			return err
		}

		if snap.Epoch != epoch {
			// adopted a newer epoch: the stream we were about to attach to is gone
			utils.Log("[OPLOG] Adopted epoch " + strconv.FormatUint(snap.Epoch, 10) + " from snapshot")
			if !oplogDropStaleJetStream() {
				afterInstallReaction(before)
			}
			return errors.New("oplog: epoch changed during fast-forward, re-attaching")
		}

		si, errInfo := jsCtx.StreamInfo(oplogStreamName(snap.Epoch))
		if errInfo != nil {
			return errInfo
		}

		// mandatory gap check: retention can move past our snapshot while we
		// install it, which would leave us permanently one entry short
		if si.State.Msgs == 0 || si.State.FirstSeq <= snap.Seq+1 {
			utils.Log("[OPLOG] Fast-forwarded to seq " + strconv.FormatUint(snap.Seq, 10))
			afterInstallReaction(before)
			return nil
		}

		utils.Warn("[OPLOG] snapshot aged out while installing (attempt " + strconv.Itoa(attempt) + "), refetching")
	}
	return errors.New("oplog: could not catch up, the log outruns snapshot installs")
}

func requestOplogSnapshot() (OplogSnapshot, error) {
	var snap OplogSnapshot

	clientConfigLock.RLock()
	conn := nc
	clientConfigLock.RUnlock()

	if conn == nil {
		return snap, errors.New("oplog: NATS client not connected")
	}

	// our epoch travels with the request so a peer behind us declines rather than
	// answering with state we would only have to throw away
	body, err := json.Marshal(oplogSnapshotRequest{Epoch: utils.GetOplogEpoch()})
	if err != nil {
		return snap, err
	}

	msg, err := conn.Request(oplogSnapshotSubject, body, oplogSnapshotTimeout)
	if err != nil {
		return snap, err
	}
	if err := json.Unmarshal(msg.Data, &snap); err != nil {
		return snap, err
	}
	return snap, nil
}

func installOplogSnapshot(snap OplogSnapshot) error {
	utils.ConfigLock.Lock()
	defer utils.ConfigLock.Unlock()

	if err := utils.ApplyLogicalDump(snap.Dump, snap.Epoch, snap.Seq); err != nil {
		return err
	}

	for name, state := range snap.Domains {
		d, ok := oplogDomains[name]
		if !ok {
			utils.Warn("[OPLOG] snapshot carries unknown domain " + name + ", ignoring")
			continue
		}
		if err := d.Apply(state); err != nil {
			return err
		}
	}
	return nil
}

// oplogDropStaleJetStream throws away the JetStream state a manager built under
// the epoch it has just been overtaken by, and restarts into the new one.
//
// A force-reform abandons a cluster: its meta-raft group and its stream stay on
// every manager's disk. A manager that was down for the reform and comes back
// would otherwise carry that group into the constellation it is rejoining, where
// it is a claim on a quorum that no longer exists. Agents run no JetStream, so
// they have nothing to drop and restart through the ordinary reaction instead.
//
// Reports whether it took the restart over. Runs async: the caller is inside the
// apply path, and the teardown stops the very loop it is running under.
//
// KNOWN RESIDUAL, so nobody rediscovers it as a bug: this only reaches a manager
// that ADOPTS a newer epoch, which means one that could not attach. Managers
// revived together in numbers that still hold quorum of the ABANDONED cluster's
// meta group elect among themselves, attach to their own old stream, and never
// re-enter oplogAttach — so they never ask for a snapshot, never see the new
// epoch, and never get here. They stay a self-consistent island at the dead
// epoch. They cannot contaminate the live cluster (no current-epoch node will
// create or adopt into their epoch, and survivors no longer dial them), and an
// operator restart kills their credentials since reform blocked their device
// records. Fencing them at the transport needs authenticated cluster routes,
// which the routes here deliberately do not have.
func oplogDropStaleJetStream() bool {
	if utils.FBL.AgentMode {
		return false
	}
	go func() {
		StopNATS()
		CloseNATSClient()
		os.RemoveAll(jetstreamDir())
		RestartNebula()
	}()
	return true
}

// afterInstallReaction bounces the mesh only when the snapshot actually moved
// the device set; a no-op catch-up costs nothing.
func afterInstallReaction(before []byte) {
	after, err := utils.BuildLogicalDump()
	if err != nil {
		utils.Error("[OPLOG] Failed to read back state after snapshot install", err)
		return
	}
	if bytes.Equal(before, after) {
		return
	}
	go RestartNebula()
}
