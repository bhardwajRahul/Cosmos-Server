package constellation

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strconv"

	"github.com/azukaar/cosmos-server/src/utils"
)

// Force-reform is the manual way out of a permanently lost quorum: an HA
// constellation that has lost two of its three managers can no longer publish
// anything, including the device edits that would replace them. It re-enters
// formation on the survivor at a new epoch, so the survivor writes directly
// again and replacement managers can be enrolled normally.
//
// Destructive and deliberately so — the old managers are fenced out, not
// recovered. Everything that isn't cluster state (CA, keys, config, devices)
// is left exactly as it is.

// blockDeadManagers blocks every manager but this one.
//
// Scope is CosmosNode == 2 on purpose. That is the definition of a cluster
// voter, and voters are the only thing a reform has to fence: a CosmosNode 0 row
// is a plain nebula client that runs no NATS server and holds no route, and
// agents (CosmosNode == 1) are deliberately spared so they can follow the
// survivor into the new epoch by snapshot.
//
// LOCAL writes, never published: the whole point of reform is that this node has
// no quorum to publish through, and a blocked-manager record is node-local truth
// until the replacements enroll and re-materialize from the new log. This is the
// leave/teardown class of write (see utils.CommitMutationLocal), and formation is
// exactly where it is legal.
func blockDeadManagers(selfName string) ([]string, error) {
	managers, err := utils.FindDevices(map[string]interface{}{"CosmosNode": 2, "Blocked": false})
	if err != nil {
		return nil, err
	}

	blocked := []string{}
	for _, d := range managers {
		if d.DeviceName == "" || d.DeviceName == selfName {
			continue
		}
		err := utils.CommitMutationLocal(utils.Mutation{
			Table:  "devices",
			Op:     "updateMany",
			Filter: map[string]interface{}{"DeviceName": d.DeviceName, "Blocked": false},
			Doc:    map[string]interface{}{"Blocked": true},
		})
		if err != nil {
			return blocked, err
		}
		blocked = append(blocked, d.DeviceName)
	}
	return blocked, nil
}

// API_ForceReform godoc
// @Summary Force the constellation to re-form around this manager
// @Description HA only. Bumps the op-log epoch, drops local JetStream state, blocks the other managers and restarts NATS, leaving this node writable in formation mode so replacements can be enrolled. Does not touch the CA, keys, config or device data.
// @Tags constellation
// @Produce json
// @Security BearerAuth
// @Success 200 {object} utils.APIResponse
// @Failure 400 {object} utils.HTTPErrorResult
// @Failure 403 {object} utils.HTTPErrorResult
// @Failure 500 {object} utils.HTTPErrorResult
// @Router /api/constellation/force-reform [post]
func API_ForceReform(w http.ResponseWriter, req *http.Request) {
	if utils.CheckPermissions(w, req, utils.PERM_ADMIN) != nil {
		return
	}

	if req.Method != "POST" {
		utils.Error("API_ForceReform: Method not allowed "+req.Method, nil)
		utils.HTTPError(w, "Method not allowed", http.StatusMethodNotAllowed, "HTTP001")
		return
	}

	// Non-HA has nothing to reform: its single manager's JetStream is always up,
	// so it is never quorum-blocked in the first place.
	if !IsNATSHA() {
		utils.Error("API_ForceReform: not an HA constellation", nil)
		utils.HTTPError(w, "Force-reform only applies to an HA constellation",
			http.StatusBadRequest, "AFR001")
		return
	}

	selfName, err := GetCurrentDeviceName()
	if err != nil || selfName == "" {
		utils.Error("API_ForceReform: cannot identify this device", err)
		utils.HTTPError(w, "Cannot identify this device", http.StatusInternalServerError, "AFR002")
		return
	}

	epoch, blocked, err := performForceReform(selfName)
	if err != nil {
		utils.Error("API_ForceReform: reform failed", err)
		utils.HTTPError(w, "Force-reform failed: "+err.Error(),
			http.StatusInternalServerError, "AFR003")
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "OK",
		"data": map[string]interface{}{
			"epoch":          epoch,
			"blockedDevices": blocked,
		},
	})
}

// performForceReform is the reform itself, callable without an HTTP request so
// the E2E harness exercises this code rather than a copy of it. Callers own the
// guards (permissions, method, HA-only); this owns the sequence.
//
// It schedules its own restart — callers must NOT trigger one.
func performForceReform(selfName string) (uint64, []string, error) {
	utils.Log("[REFORM] Re-forming the constellation around " + selfName)

	// One tx, and it comes first: the epoch bump is the fence. Everything after it
	// is cleanup a crash can safely repeat, but a node that tore down its cluster
	// state while still claiming the old epoch could rejoin the log it just left.
	epoch, err := utils.ReformOplogEpoch()
	if err != nil {
		return 0, nil, errors.New("could not start a new op-log epoch: " + err.Error())
	}
	// the new epoch has no log yet, which is what re-opens the direct write path
	oplogStreamSeen.Store(false)

	// Unconditional, and deferred from here rather than run at the end: every step
	// below stops something (the apply loop, NATS, the client), so returning early
	// on an error without this would leave the node with its epoch bumped, NATS
	// down, and nothing scheduled to bring any of it back — recoverable only by
	// restarting the process.
	defer func() { go RestartNebula() }()

	StopOplogApply()
	StopNATS()
	CloseNATSClient()

	// ONLY the JetStream directory: the streams and meta-raft group belong to the
	// cluster we are abandoning. The CA, the node keys, nebula.yml, config.json and
	// the device rows are all still valid and are what the new epoch is seeded from.
	if err := os.RemoveAll(jetstreamDir()); err != nil {
		return epoch, nil, errors.New("could not remove local JetStream state: " + err.Error())
	}

	blocked, err := blockDeadManagers(selfName)
	if err != nil {
		return epoch, blocked, errors.New("could not block the departed managers: " + err.Error())
	}

	// so the restart builds cluster routes and NATS users from the survivors only:
	// routes come from getManagerIPs, which subtracts the blocked set, and the NATS
	// user list is rebuilt from this cache — which is how the fenced managers'
	// credentials die
	refreshDeviceCache()

	utils.Log("[REFORM] Epoch is now " + strconv.FormatUint(epoch, 10) +
		", managers blocked: " + strconv.Itoa(len(blocked)))
	return epoch, blocked, nil
}
