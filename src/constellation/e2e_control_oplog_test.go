//go:build e2e

package constellation

// Control-API surface added for the op-log and M3 reform scenarios, split out of
// e2e_nodemain_test.go to keep that file within the repo's size limit. These
// handlers run inside the node child process, so they can call package-internal
// code (DeviceCreate, connectNATSClient, API_ForceReform) directly rather than
// reimplementing it in the parent test.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/azukaar/cosmos-server/src/utils"
)

// e2eWriteJSON / e2eFail / e2eFailStore are shared with e2eControlServer, which
// aliases them; they are package-level so handlers can live in either file.
func e2eWriteJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func e2eFail(w http.ResponseWriter, err error) {
	e2eWriteJSON(w, 500, map[string]interface{}{"error": err.Error()})
}

// e2eFailStore surfaces the op-log write contract to the parent test. e2eFail
// collapses everything to 500, which would make "publish failed" and "apply
// timed out" indistinguishable — and a regression turning every 409 into a 503
// would sail straight through a test that can only assert "the write failed".
func e2eFailStore(w http.ResponseWriter, err error) {
	status, code := 500, "internal"
	var ec *utils.ErrConstraint
	switch {
	case errors.Is(err, utils.ErrReadOnly):
		status, code = 409, "read-only"
	case errors.Is(err, utils.ErrApplyTimeout):
		status, code = 503, "apply-timeout"
	case errors.As(err, &ec):
		status, code = 409, "duplicate"
	case errors.Is(err, utils.ErrNotFound):
		status, code = 404, "not-found"
	}
	e2eWriteJSON(w, status, map[string]interface{}{
		"status": status, "code": code, "error": err.Error(),
	})
}

// e2eRegisterOplogControl adds the op-log and reform endpoints to the node's
// control mux.
func e2eRegisterOplogControl(mux *http.ServeMux) {
	writeJSON := e2eWriteJSON
	fail := e2eFail
	failStore := e2eFailStore

	// break-oplog-consumer kills the live subscription while deliberately LEAVING
	// oplogAttached set and oplogSub non-nil.
	//
	// That combination is not a simulation, it is the exact state a real quorum
	// outage was observed to leave behind (see TestE2EOplogDeadConsumerRecovery):
	// the ordered consumer is gone, but the stream still exists at the same epoch
	// and StreamInfo answers normally, so neither of the liveness probe's original
	// detach conditions can see it. The node publishes into a log it will never
	// read back.
	//
	// It exists because the natural trigger is a race inside JetStream — it
	// reproduced once in seven full-suite runs — and a defect that severe cannot
	// have its only coverage be a coin flip. Reproducing the STATE is deterministic;
	// reproducing the outage that sometimes causes it is not.
	mux.HandleFunc("/break-oplog-consumer", func(w http.ResponseWriter, r *http.Request) {
		oplogMu.Lock()
		sub := oplogSub
		oplogMu.Unlock()

		if sub == nil {
			writeJSON(w, 200, map[string]interface{}{
				"broken": false, "reason": "no active subscription",
			})
			return
		}
		// Unsubscribe only. Clearing the latch or nilling oplogSub would hand the
		// supervisor the answer the probe is supposed to work out for itself.
		if err := sub.Unsubscribe(); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]interface{}{
			"broken":   true,
			"attached": oplogAttached.Load(),
			"seq":      utils.GetLastAppliedSeq(),
		})
	})

	// stream-info exposes the op-log stream's actual replication. Nothing else can
	// see it, and without it two things are untestable: that R1 is raised to R3
	// once the third manager is up, and — the reason this was added — that a
	// "kill two of three managers" scenario is actually testing quorum loss. While
	// the stream is still R1 it lives on a single server, so if that server is the
	// survivor its writes keep succeeding and the scenario silently tests nothing.
	mux.HandleFunc("/stream-info", func(w http.ResponseWriter, r *http.Request) {
		clientConfigLock.RLock()
		jsl := js
		clientConfigLock.RUnlock()
		if jsl == nil {
			fail(w, errNoJS)
			return
		}
		name := oplogStreamName(utils.GetOplogEpoch())
		si, err := jsl.StreamInfo(name)
		if err != nil {
			writeJSON(w, 200, map[string]interface{}{
				"exists": false, "name": name, "error": err.Error(),
			})
			return
		}
		// configured replicas is what was asked for; the peer count is what JetStream
		// actually placed, and only the second one means the data is really replicated
		peers, leader := 0, ""
		if si.Cluster != nil {
			peers = len(si.Cluster.Replicas) + 1
			leader = si.Cluster.Leader
		}
		writeJSON(w, 200, map[string]interface{}{
			"exists": true, "name": name,
			"replicas": si.Config.Replicas, "peers": peers, "leader": leader,
			"msgs": si.State.Msgs, "firstSeq": si.State.FirstSeq, "lastSeq": si.State.LastSeq,
		})
	})

	// enroll runs the real enrollment path in THIS node's process: DeviceCreate
	// plus the client YAML the new node boots from. A replacement manager joining
	// after a force-reform has to be enrolled by the survivor — enrolling it in
	// the parent test process would skip the survivor's post-reform device table
	// and its new epoch, which is the part under test.
	mux.HandleFunc("/enroll", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			DeviceName   string
			IP           string
			CosmosNode   int
			IsLighthouse bool
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			fail(w, err)
			return
		}

		cert, key, _, req, err := DeviceCreate(DeviceCreateRequestJSON{
			DeviceName:   body.DeviceName,
			IP:           body.IP,
			IsLighthouse: body.IsLighthouse,
			CosmosNode:   body.CosmosNode,
			Port:         "4242",
		})
		if err != nil {
			failStore(w, err)
			return
		}

		capki, err := getCApki()
		if err != nil {
			fail(w, err)
			return
		}

		yml, err := getYAMLClientConfig(body.DeviceName, utils.CONFIGFOLDER+"nebula.yml",
			capki, cert, key, req.APIKey, utils.ConstellationDevice{
				DeviceName:   body.DeviceName,
				IP:           req.IP,
				IsLighthouse: body.IsLighthouse,
				CosmosNode:   body.CosmosNode,
				Port:         "4242",
				APIKey:       req.APIKey,
			}, false, false)
		if err != nil {
			fail(w, err)
			return
		}

		writeJSON(w, 200, map[string]interface{}{
			"nebulaYml": yml,
			"apiKey":    req.APIKey,
		})
	})

	// edit-device mutates the local device DB the way the edit API does: under
	// the op-log that publishes one entry, which every node applies in order
	mux.HandleFunc("/edit-device", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ DeviceName, Nickname string }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			fail(w, err)
			return
		}
		err := utils.UpdateDevices(
			map[string]interface{}{"DeviceName": body.DeviceName},
			map[string]interface{}{"Nickname": body.Nickname})
		if err != nil {
			failStore(w, err)
			return
		}
		writeJSON(w, 200, map[string]interface{}{"ok": true, "status": 200})
	})

	mux.HandleFunc("/block-device", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			DeviceName string
			Blocked    bool
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			fail(w, err)
			return
		}
		err := utils.UpdateDevices(
			map[string]interface{}{"DeviceName": body.DeviceName},
			map[string]interface{}{"Blocked": body.Blocked})
		if err != nil {
			failStore(w, err)
			return
		}
		writeJSON(w, 200, map[string]interface{}{"ok": true, "status": 200})
	})

	// create-device: ConflictingWrites races two nodes on the same name/IP, and
	// SnapshotFastForward needs bulk writes to push the log past its 100-entry
	// retention. The existing endpoints can only mutate pre-enrolled devices.
	mux.HandleFunc("/create-device", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ DeviceName, IP string }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			fail(w, err)
			return
		}
		err := utils.CreateDevice(utils.ConstellationDevice{
			Nickname:   body.DeviceName,
			DeviceName: body.DeviceName,
			IP:         body.IP,
			APIKey:     "e2e-" + body.DeviceName,
		})
		if err != nil {
			failStore(w, err)
			return
		}
		writeJSON(w, 200, map[string]interface{}{"ok": true, "status": 200})
	})

	// create-user: the PRIMARY KEY rejection path (users are keyed by nickname, so
	// a duplicate arrives as a different SQLite code than a duplicate device does).
	mux.HandleFunc("/create-user", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Nickname string }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			fail(w, err)
			return
		}
		err := utils.CreateUser(utils.User{Nickname: body.Nickname, Password: "e2e-hash"})
		if err != nil {
			failStore(w, err)
			return
		}
		writeJSON(w, 200, map[string]interface{}{"ok": true, "status": 200})
	})

	// create-users bulk-writes N users in one request. SnapshotFastForward has to
	// push the log past its 100-entry retention while a node is down, and users are
	// the only affordable way: a device op bounces nebula on every node, so 120
	// device writes would be 120 mesh restarts. Server-side looping also keeps the
	// burst from being dominated by HTTP round trips.
	mux.HandleFunc("/create-users", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Prefix string
			Count  int
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			fail(w, err)
			return
		}
		created := 0
		for i := 0; i < body.Count; i++ {
			err := utils.CreateUser(utils.User{
				Nickname: fmt.Sprintf("%s-%04d", body.Prefix, i),
				Password: "e2e-hash",
			})
			if err != nil {
				// partial progress is the useful part of the failure: it says whether
				// the burst died on write 1 or write 119
				writeJSON(w, 500, map[string]interface{}{
					"created": created, "failedAt": i, "error": err.Error(),
				})
				return
			}
			created++
		}
		writeJSON(w, 200, map[string]interface{}{"created": created})
	})

	// probe-nats answers "would these credentials still get in?" directly, instead
	// of inferring revocation from something downstream. Credential fencing is
	// asserted nowhere else: a node's own client always dials its own server
	// (InitNATSClient builds nats://<own IP>:4222), so a fenced node still reports
	// clientConnected — the only honest test is to dial a *survivor* with the
	// fenced identity and see it refused.
	mux.HandleFunc("/probe-nats", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ URL, User, Password string }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			fail(w, err)
			return
		}
		conn, err := connectNATSClient(body.URL, body.User, body.Password)
		if err != nil {
			writeJSON(w, 200, map[string]interface{}{"connected": false, "error": err.Error()})
			return
		}
		conn.Close()
		writeJSON(w, 200, map[string]interface{}{"connected": true})
	})

	// force-reform drives the REAL handler, API_ForceReform, rather than a copy of
	// its steps: the request is re-issued carrying an admin AuthContext so
	// CheckPermissions passes. Reimplementing the reform sequence here would test
	// the harness's copy of it and would silently stop testing the product the
	// first time the two drifted — and reform is precisely the operation where a
	// missed step (the epoch bump, the jetstream wipe, blocking the dead managers)
	// is invisible until a stale node turns up months later.
	mux.HandleFunc("/force-reform", func(w http.ResponseWriter, r *http.Request) {
		admin := &utils.AuthContext{
			Nickname:    "e2e-admin",
			Permissions: []utils.Permission{utils.PERM_ADMIN},
			IsSudoed:    true,
		}
		req := r.Clone(context.WithValue(r.Context(), utils.AuthCtxKey, admin))
		req.Method = "POST"
		API_ForceReform(w, req)
	})
}
