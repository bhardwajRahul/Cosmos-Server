package constellation

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/azukaar/cosmos-server/src/utils"
)

// The config op-log: one ordered JetStream stream is the single source of truth
// for users, devices and the config domains below. Every node — manager and
// agent alike — materializes its local state by applying that stream in order,
// so there is no conflict resolution anywhere: ops simply apply in stream order
// on every replica.

const oplogEnvVersion = 1

const (
	// publish must ack within this or the node reports itself read-only
	oplogPublishTimeout = 5 * time.Second
	// how long a writer waits to see its own op come back through the apply loop
	oplogApplyWait = 10 * time.Second
	// retention: the log is a catch-up buffer, not history — fall behind and you snapshot
	oplogMaxMsgs = 100
)

// OpEnvelope is one entry in the log. Domain payloads carry the FULL domain
// state with op "set", which makes them idempotent and last-writer-wins in
// stream order; db payloads carry a filter+doc mutation.
type OpEnvelope struct {
	V      int                    `json:"v"`
	Epoch  uint64                 `json:"epoch"`
	Kind   string                 `json:"kind"` // "db" | "domain"
	Table  string                 `json:"table,omitempty"`
	Domain string                 `json:"domain,omitempty"`
	Op     string                 `json:"op"`
	PK     string                 `json:"pk,omitempty"` // informational, for log reading
	Filter map[string]interface{} `json:"filter,omitempty"`
	Doc    json.RawMessage        `json:"doc,omitempty"`
	Origin string                 `json:"origin"`
	ReqID  string                 `json:"reqId"`
}

func oplogStreamName(epoch uint64) string {
	return "cosmos-oplog-e" + strconv.FormatUint(epoch, 10)
}

func oplogSubjectPrefix(epoch uint64) string {
	return "cosmos._global_.oplog.e" + strconv.FormatUint(epoch, 10) + "."
}

func oplogSubjectWildcard(epoch uint64) string {
	return oplogSubjectPrefix(epoch) + ">"
}

// subject tokens can't carry '.' or the domain wildcards; the envelope stays authoritative
func sanitizeSubjectToken(s string) string {
	r := strings.NewReplacer(".", "_", " ", "_", "*", "_", ">", "_", ":", "_")
	if s == "" {
		return "_"
	}
	return r.Replace(s)
}

func oplogSubject(env OpEnvelope) string {
	target := env.Table
	if env.Kind == "domain" {
		target = env.Domain
	}
	return oplogSubjectPrefix(env.Epoch) + env.Kind + "." + sanitizeSubjectToken(target)
}

func oplogOrigin() string {
	name, err := GetCurrentDeviceName()
	if err != nil {
		return ""
	}
	return name
}

// oplogMode is where a write goes on this node right now.
type oplogMode int

const (
	// single-writer by construction — safe to write straight to SQLite
	oplogDirect oplogMode = iota
	oplogPublish
	oplogReadOnly
)

// oplogWriteMode decides the write path. Availability falls out of the topology
// rather than the node's role: whoever cannot reach a writable stream is
// read-only, whoever is provably the only writer goes direct.
func oplogWriteMode() oplogMode {
	// Self-fencing, and it has to come before everything else including the
	// attached fast path — a fenced node is at its most dangerous precisely when
	// it looks healthy.
	//
	// A force-reform blocks the managers it removes, but blocking only revokes
	// their CLIENT credentials, and a manager's client dials its OWN server. The
	// cluster route listener has no user authentication at all, so a revived
	// manager re-establishes routes, rejoins the meta group, adopts the current
	// epoch by snapshot and writes — observed end to end in TestE2EStaleManagerFenced.
	// Consistency survives all of that (it applies the real log in order); what
	// does not survive is the operator's decision to remove it.
	//
	// So a node that can see it has been removed stops writing. This works because
	// the snapshot it adopts CONTAINS its own device record marked blocked.
	// Cooperative rather than enforced: a node that never reaches the cluster never
	// learns, and only transport-level auth can actually exclude it.
	if isSelfBlocked() {
		return oplogReadOnly
	}

	// Checked first among the rest on purpose: once we hold a live log, writes
	// publish no matter what else looks true.
	if oplogAttached.Load() {
		return oplogPublish
	}

	// Constellation off: no log exists for this node at all, so direct is the only
	// path. Also the zero-overhead fast path for single-node installs, and the exit
	// ResetNebula takes — it disables constellation but leaves a stale sequence in
	// kv, so this check must come before the one below or such a node would be
	// read-only forever with no way back.
	if !utils.GetMainConfig().ConstellationConfig.Enabled {
		return oplogDirect
	}

	// Fail closed: a node that has materialized from this log must never write
	// outside it again. Both direct branches below are reachable during an ordinary
	// RestartNebula — stop() clears NebulaStarted and the device cache together
	// (nebula.go), which makes a real cluster member look both stopped and
	// standalone — and the apply loop's reaction to a device write is
	// `go RestartNebula()`. Without this guard the seconds after every device write
	// are a window where a second write lands locally, is never published, and is
	// silently discarded by the next snapshot or domain op.
	//
	// Keyed on the bootstrapped marker, NOT on last_applied_seq > 0. A node that
	// joined by installing a founder's snapshot legitimately sits at seq 0 with a
	// full store, so the sequence test let exactly those nodes through to a direct
	// write — an agent's create-device returned 200 having only ever written
	// locally, and a two-node race saw both writers "win".
	if utils.IsOplogBootstrapped() {
		return oplogReadOnly
	}

	// Genuinely pre-log from here down: fresh single-node install...
	if IsConstellationStandalone() {
		return oplogDirect
	}
	// ...or pre-NATS boot, where migrations and new-install setup run before any
	// peer is reachable.
	if !NebulaStarted.Load() {
		return oplogDirect
	}

	// Formation. An HA constellation has no JetStream until its second manager
	// enrolls, so the creator (and the survivor of a force-reform) writes straight
	// to SQLite until a log exists to publish into. Single-writer by construction:
	// the licence is granted to exactly one node, enrollment happens there, and
	// every other node's publish finds no stream and comes back read-only.
	//
	// Both halves matter. The licence alone is not enough — it ends the instant a
	// log exists, whether or not this node has attached to it yet, because a direct
	// write after stream creation would never reach the log and would be erased by
	// the first snapshot.
	if utils.IsFormationWriter() && !oplogStreamSeen.Load() {
		return oplogDirect
	}
	return oplogReadOnly
}

// isSelfBlocked reports whether this node's own device record marks it blocked,
// i.e. whether the constellation has removed it.
//
// Read from the store rather than the device cache: refreshDeviceCache drops
// blocked devices entirely, and GetCurrentDevice falls back to nebula.yml, so the
// cache can never say "you are blocked" — only "you are absent", which is also
// what a not-yet-enrolled node looks like.
//
// Fails OPEN on every uncertainty (no name, no record, read error): a false
// positive takes a healthy node read-only for a transient DB problem, while a
// false negative is only the status quo this fixes. An absent record is NOT
// blocked — that is a node mid-enrollment.
func isSelfBlocked() bool {
	name, err := GetCurrentDeviceName()
	if err != nil || name == "" {
		return false
	}
	device, err := utils.GetDeviceByName(name, false)
	if err != nil {
		return false
	}
	return device.Blocked
}

// publishOp is the utils.CommitMutation hook: every user/device write in the
// product funnels through here.
func publishOp(m utils.Mutation) error {
	switch oplogWriteMode() {
	case oplogDirect:
		// reactions live in the apply loop, so the direct path has to fire them itself
		var pre []utils.ConstellationDevice
		if err := utils.CommitMutationDirect(m, &pre); err != nil {
			return err
		}
		oplogReactToTableOp(m, pre)
		return nil
	case oplogReadOnly:
		// best-effort ops are dropped rather than written locally: a local-only
		// write would permanently diverge this node's materialization
		if m.BestEffort {
			utils.Debug("[OPLOG] dropping best-effort " + m.Table + " op, log not writable")
			return nil
		}
		return utils.ErrReadOnly
	}

	doc, err := utils.EncodeOpDoc(m)
	if err != nil {
		return err
	}
	filter, err := utils.NormalizeOpFilter(m.Table, m.Filter)
	if err != nil {
		return err
	}

	return publishEnvelope(OpEnvelope{
		V:      oplogEnvVersion,
		Epoch:  utils.GetOplogEpoch(),
		Kind:   "db",
		Table:  m.Table,
		Op:     m.Op,
		Filter: filter,
		Doc:    doc,
		Origin: oplogOrigin(),
		ReqID:  utils.GenerateRandomString(24),
	}, m.BestEffort)
}

// PublishDomainOp publishes the full state of a config domain and waits for the
// apply loop to install it locally. Callers must NOT hold utils.ConfigLock —
// the apply path takes it.
func PublishDomainOp(domain string, state interface{}) error {
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}

	switch oplogWriteMode() {
	case oplogDirect:
		return applyDomainLocal(domain, raw)
	case oplogReadOnly:
		return utils.ErrReadOnly
	}

	return publishEnvelope(OpEnvelope{
		V:      oplogEnvVersion,
		Epoch:  utils.GetOplogEpoch(),
		Kind:   "domain",
		Domain: domain,
		Op:     "set",
		Doc:    raw,
		Origin: oplogOrigin(),
		ReqID:  utils.GenerateRandomString(24),
	}, false)
}

func publishEnvelope(env OpEnvelope, bestEffort bool) error {
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}

	// register before publishing: the apply loop can deliver our own op back
	// before js.Publish has even returned
	var wait chan error
	if !bestEffort {
		wait = registerOpWaiter(env.ReqID)
		defer releaseOpWaiter(env.ReqID)
	}

	clientConfigLock.RLock()
	jsCtx := js
	clientConfigLock.RUnlock()

	if jsCtx == nil {
		return utils.ErrReadOnly
	}

	// epoch-in-subject is the publish fence: a stale-epoch node's subject matches
	// no stream, so it gets no ack and can never write
	_, err = jsCtx.Publish(oplogSubject(env), data, nats.MsgId(env.ReqID), nats.AckWait(oplogPublishTimeout))
	if err != nil {
		utils.Warn("[OPLOG] publish rejected (" + env.Kind + "/" + env.Table + env.Domain + "): " + err.Error())
		if bestEffort {
			return nil
		}
		return utils.ErrReadOnly
	}

	if bestEffort {
		return nil
	}

	select {
	case applyErr := <-wait:
		return applyErr
	case <-time.After(oplogApplyWait):
		return utils.ErrApplyTimeout
	}
}

// waiter registry: an op's originator blocks until its own entry comes back
// through the apply loop, so an HTTP handler returns only once the write is real.
var opWaitersMu sync.Mutex
var opWaiters = map[string]chan error{}

func registerOpWaiter(reqID string) chan error {
	ch := make(chan error, 1)
	opWaitersMu.Lock()
	opWaiters[reqID] = ch
	opWaitersMu.Unlock()
	return ch
}

func releaseOpWaiter(reqID string) {
	opWaitersMu.Lock()
	delete(opWaiters, reqID)
	opWaitersMu.Unlock()
}

func notifyOpWaiter(reqID string, err error) {
	opWaitersMu.Lock()
	ch, ok := opWaiters[reqID]
	opWaitersMu.Unlock()
	if !ok {
		return
	}
	select {
	case ch <- err:
	default:
	}
}

// createOplogStream creates the log for this epoch, and reports whether THIS
// call is the one that made it. Agents never create — they wait for a manager.
//
// Callers must have established that no stream exists for the epoch AND that no
// peer offered a snapshot first (see oplogAdoptFromPeer): by the time we are
// here, "nobody has this log" is the conclusion, not the assumption.
//
// created is not bookkeeping — it is the only local evidence of which node
// seeded the log, and it is what licenses oplogAttach to mark this node
// bootstrapped from its own SQLite.
func createOplogStream(jsCtx nats.JetStreamContext, epoch uint64, waitingSince time.Time) (*nats.StreamInfo, bool, error) {
	name := oplogStreamName(epoch)

	if err := oplogMayCreateStream(name, waitingSince); err != nil {
		return nil, false, err
	}

	utils.Log("[OPLOG] Creating stream " + name)

	// R1 at creation even in HA: a three-way replica group cannot be placed while
	// the cluster is still forming. It is raised to R3 by oplogMaintainReplicas
	// once the third manager is up.
	si, err := jsCtx.AddStream(&nats.StreamConfig{
		Name:     name,
		Subjects: []string{oplogSubjectWildcard(epoch)},
		Storage:  nats.FileStorage,
		MaxMsgs:  oplogMaxMsgs,
		Discard:  nats.DiscardOld,
		Replicas: 1,
	})
	if err != nil {
		// lost the race with another manager — its stream is just as good
		if si2, err2 := jsCtx.StreamInfo(name); err2 == nil {
			return si2, false, nil
		}
		return nil, false, err
	}
	return si, true, nil
}

// oplogMayCreateStream decides whether this node is allowed to create the log
// right now, and is the whole of createOplogStream's policy.
//
// Every "no" here is a DEFERRAL that times out, never a veto. That distinction
// is the difference between deferring to a better-qualified creator and bricking
// a constellation: an install upgrading from a release that predates the op-log
// holds no formation licence and no bootstrapped marker, and nothing will ever
// grant it either, so a permanent refusal would leave every node read-only with
// no operator action that recovers it.
func oplogMayCreateStream(name string, waitingSince time.Time) error {
	if utils.FBL.AgentMode {
		return errors.New("oplog: stream " + name + " does not exist yet")
	}

	// Formation exit: the formation writer holds the seed state, so it creates the
	// log the moment JetStream elects — ahead of the election below, which could
	// otherwise hand creation to a peer that has nothing to seed with and leave the
	// writer's rows out of the log entirely.
	if utils.IsFormationWriter() {
		return nil
	}

	// A node that HAS materialized from a log may never create one. This is a
	// refusal, not a deferral, and it is the single most important line here.
	//
	// Its own epoch having no stream means the cluster moved past it — a
	// force-reform renames the log — so the only correct move is to adopt what a
	// peer reports. Creating instead RESURRECTS the abandoned epoch's stream on the
	// live cluster, and the publish fence is built entirely on that stream not
	// existing: the old cluster's nodes would start getting acks, applying, and
	// reporting success, leaving two live streams and two disjoint materializations
	// that both look healthy. Being bootstrapped used to SKIP the checks below,
	// which is how a node that adopted a stale epoch from a revived peer could go
	// on to recreate that epoch's log here.
	//
	// It also makes losing a snapshot reply race harmless: if a stale peer answers
	// first, we decline to create, stay read-only, and the supervisor asks again on
	// the next tick until a current peer answers. Delay instead of divergence.
	if utils.IsOplogBootstrapped() {
		// Standalone cannot fork by definition — there is no peer holding another
		// materialization — so a single node whose store dir was lost can rebuild.
		if IsConstellationStandalone() {
			return nil
		}
		return errors.New("oplog: " + name + " is missing and this node has already materialized " +
			"from a log; waiting to adopt a peer's epoch rather than recreating it")
	}

	// Genuinely pre-op-log from here down: a fresh founder, or an install upgrading
	// from a release that predates the op-log. Deferral, never veto — such an
	// install holds no licence, nothing will ever grant it one, and a permanent
	// refusal would leave every node read-only with no operator recovery.
	if IsConstellationStandalone() {
		return nil
	}
	if time.Since(waitingSince) < kvCreatorFallbackAfter {
		// long enough for any peer holding real state to answer a snapshot request
		// and for the formation writer of a normal creation to get there first
		return errors.New("oplog: waiting for a node holding materialized state to create " + name)
	}
	// After the window anyone may create; AddStream has exactly one winner and the
	// losers see created == false and snapshot from it. Deliberately not gated on
	// designatedKVCreator: on an upgraded install that election's winner may be a
	// node that is never coming back, and nothing else would then create the log.
	return nil
}

// oplogTargetReplicas: R3 once the constellation is HA and has the three
// managers to place them on, R1 while it is smaller than that.
func oplogTargetReplicas() int {
	if IsNATSHA() && countKnownManagers() >= 3 {
		return 3
	}
	return 1
}

// countKnownManagers counts distinct live managers in the device cache. The
// cache indexes each device under its name AND every public hostname, so the
// count has to be over names rather than entries.
func countKnownManagers() int {
	devices, _ := deviceCacheSnapshot()
	names := map[string]bool{}
	for _, d := range devices {
		if d.CosmosNode == 2 && d.DeviceName != "" && !d.Blocked {
			names[d.DeviceName] = true
		}
	}
	return len(names)
}

// oplogReplicaSettledFor paces the probe once the log is already the right size.
// Only the settled path is paced: while a scale-up is actually outstanding the
// supervisor's own 2s tick drives it, because placement only becomes possible
// some seconds after the third manager's cluster route comes up.
const oplogReplicaSettledFor = 20 * time.Second

var oplogReplicaMu sync.Mutex
var oplogReplicaSettledAt time.Time
var oplogReplicaWantedSince time.Time

func oplogReplicaSettle() {
	oplogReplicaMu.Lock()
	oplogReplicaSettledAt = time.Now()
	oplogReplicaWantedSince = time.Time{}
	oplogReplicaMu.Unlock()
}

func oplogReplicaIsSettled() bool {
	oplogReplicaMu.Lock()
	defer oplogReplicaMu.Unlock()
	return time.Since(oplogReplicaSettledAt) < oplogReplicaSettledFor
}

// oplogReplicaWaitedForCreator reports whether we have wanted R3 for long enough
// that the designated creator has to be presumed gone. Without this the election
// is a single point of failure exactly where HA was promised: countKnownManagers
// counts cache entries rather than liveness, so if the lowest-named manager is
// the one that died, every remaining manager computes a target of R3, defers to
// a node that is not coming back, and the log silently stays R1 forever.
func oplogReplicaWaitedForCreator() bool {
	oplogReplicaMu.Lock()
	defer oplogReplicaMu.Unlock()
	if oplogReplicaWantedSince.IsZero() {
		oplogReplicaWantedSince = time.Now()
	}
	return time.Since(oplogReplicaWantedSince) >= kvCreatorFallbackAfter
}

// oplogMaintainReplicas scales the log up to R3 once the third manager enrolls.
// UpdateStream, never drop-and-recreate: this is durable state, and recreating
// it would restart the sequence out from under every node's last_applied_seq.
func oplogMaintainReplicas() {
	if utils.FBL.AgentMode || !IsNATSHA() || oplogTargetReplicas() < 3 {
		return
	}
	if oplogReplicaIsSettled() {
		return
	}
	if _, isSelf := designatedKVCreator(); !isSelf && !oplogReplicaWaitedForCreator() {
		return
	}

	clientConfigLock.RLock()
	jsCtx := js
	clientConfigLock.RUnlock()
	if jsCtx == nil {
		return
	}

	name := oplogStreamName(utils.GetOplogEpoch())
	si, err := jsCtx.StreamInfo(name)
	if err != nil {
		return
	}
	if si.Config.Replicas >= 3 {
		oplogReplicaSettle()
		return
	}

	cfg := si.Config
	cfg.Replicas = 3
	if _, err := jsCtx.UpdateStream(&cfg); err != nil {
		// expected while the cluster is still placing the new manager; retried next tick
		utils.Debug("[OPLOG] " + name + " not ready to scale to 3 replicas yet: " + err.Error())
		return
	}
	oplogReplicaSettle()
	utils.Log("[OPLOG] Scaled " + name + " to 3 replicas")
}
