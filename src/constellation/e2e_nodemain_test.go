//go:build e2e

package constellation

// Child-process entrypoint for the Tier-A E2E harness. The parent test
// re-execs this test binary with -test.run=^TestE2ENodeMain$ and the
// COSMOS_E2E_* environment set (see e2e_harness_test.go). The child runs the
// real constellation Init() against its own CONFIGFOLDER and exposes a small
// control HTTP API on its loopback "mesh" IP for the parent to drive and
// observe scenarios.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"sort"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/azukaar/cosmos-server/src/utils"
)

var errNoJS = errors.New("JetStream context not initialized")

func TestE2ENodeMain(t *testing.T) {
	if os.Getenv("COSMOS_E2E_NODE") != "1" {
		t.Skip("e2e harness child entrypoint, not a test")
	}

	utils.CONFIGFOLDER = os.Getenv("COSMOS_E2E_CONFIGFOLDER")

	// dead URL: licence traffic must never leave the process
	utils.FBL = utils.NewFirebaseApiSdk("http://127.0.0.1:9")
	utils.FBL.AgentMode = os.Getenv("COSMOS_E2E_AGENT") == "true"
	utils.FBL.LValid = true
	utils.FBL.CosmosNodeNumber = 100
	utils.FBL.UserNumber = 100

	utils.LoadBaseMainConfig(utils.ReadConfigFromFile())

	if err := utils.InitStore(); err != nil {
		t.Fatal("e2e node: InitStore:", err)
	}

	// control API first, so the parent can observe startup states too
	quit := make(chan struct{})
	go e2eControlServer(os.Getenv("COSMOS_E2E_CONTROL_ADDR"), quit)

	Init()

	<-quit
}

func e2eControlServer(addr string, quit chan struct{}) {
	mux := http.NewServeMux()

	// defined at package level in e2e_control_oplog_test.go so handlers can live
	// in either file
	writeJSON := e2eWriteJSON
	fail := e2eFail
	failStore := e2eFailStore

	e2eRegisterOplogControl(mux)

	// fast, no network calls — safe to poll aggressively
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		name, _ := GetCurrentDeviceName()
		leafs := -1
		routes := -1
		if srv := ns.Load(); srv != nil {
			leafs = srv.NumLeafNodes()
			routes = srv.NumRoutes()
		}
		// nebulaPid changing across an op is how the parent tells a mesh restart
		// from a plain device-cache refresh
		nebulaPid := -1
		ProcessMux.Lock()
		if process != nil && process.Process != nil {
			nebulaPid = process.Process.Pid
		}
		ProcessMux.Unlock()

		writeJSON(w, 200, map[string]interface{}{
			"name":            name,
			"agent":           utils.FBL.AgentMode,
			"nebulaStarted":   NebulaStarted.Load(),
			"natsStarted":     NATSStarted.Load(),
			"clientConnected": IsClientConnected(),
			"standalone":      IsConstellationStandalone(),
			"goroutines":      runtime.NumGoroutine(),
			"leafs":           leafs,
			"routes":          routes,
			"nebulaPid":       nebulaPid,
		})
	})

	// JetStream readiness — may block a few seconds while probing.
	// ClientConnectToJS alone is not authoritative (it recreates the JS
	// context without a round-trip), so verify with a real AccountInfo.
	mux.HandleFunc("/js", func(w http.ResponseWriter, r *http.Request) {
		ready := false
		if ClientConnectToJS() == nil {
			clientConfigLock.RLock()
			jsl := js
			clientConfigLock.RUnlock()
			if jsl != nil {
				_, err := jsl.AccountInfo(nats.MaxWait(4 * time.Second))
				ready = err == nil
			}
		}
		writeJSON(w, 200, map[string]interface{}{"ready": ready})
	})

	// raw route/leaf state from the embedded server's monitoring API
	mux.HandleFunc("/routez", func(w http.ResponseWriter, r *http.Request) {
		srv := ns.Load()
		if srv == nil {
			fail(w, errNoJS)
			return
		}
		routez, errR := srv.Routez(nil)
		leafz, errL := srv.Leafz(nil)
		if errR != nil || errL != nil {
			writeJSON(w, 500, map[string]interface{}{"routezErr": fmt.Sprint(errR), "leafzErr": fmt.Sprint(errL)})
			return
		}
		writeJSON(w, 200, map[string]interface{}{"routez": routez, "leafz": leafz})
	})

	mux.HandleFunc("/heartbeats", func(w http.ResponseWriter, r *http.Request) {
		clientConfigLock.RLock()
		jsl := js
		clientConfigLock.RUnlock()
		if jsl == nil {
			fail(w, errNoJS)
			return
		}
		kv, err := jsl.KeyValue("constellation-nodes")
		if err != nil {
			fail(w, err)
			return
		}
		keys, err := kv.Keys()
		if err != nil {
			// an empty bucket is reported as an error by the client
			writeJSON(w, 200, map[string]interface{}{"names": []string{}})
			return
		}
		writeJSON(w, 200, map[string]interface{}{"names": keys})
	})

	mux.HandleFunc("/db", func(w http.ResponseWriter, r *http.Request) {
		devices, err := GetAllDevicesEvenBlocked()
		if err != nil {
			fail(w, err)
			return
		}
		list := []map[string]interface{}{}
		for _, d := range devices {
			list = append(list, map[string]interface{}{
				"deviceName": d.DeviceName,
				"nickname":   d.Nickname,
				"ip":         d.IP,
				"cosmosNode": d.CosmosNode,
				"blocked":    d.Blocked,
			})
		}
		writeJSON(w, 200, map[string]interface{}{
			"hash":    e2eDumpHash(),
			"devices": list,
		})
	})

	mux.HandleFunc("/request", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Topic, Payload string }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			fail(w, err)
			return
		}
		resp, err := SendNATSMessage(body.Topic, body.Payload)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]interface{}{"response": resp})
	})

	mux.HandleFunc("/publish", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Topic, Payload string }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			fail(w, err)
			return
		}
		if err := PublishNATSMessage(body.Topic, body.Payload); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]interface{}{"ok": true})
	})

	// op-log position: nodes have converged when their dumps AND their
	// sequences match, which is stricter than matching dumps alone
	mux.HandleFunc("/oplog", func(w http.ResponseWriter, r *http.Request) {
		st := GetNATSStatus()
		writeJSON(w, 200, map[string]interface{}{
			"attached":       st.OplogAttached,
			"halted":         st.OplogHalted,
			"haltReason":     st.OplogHaltReason,
			"epoch":          st.OplogEpoch,
			"seq":            st.OplogSeq,
			"configWritable": st.ConfigWritable,
			// formation is the one state where a write may legally bypass the log,
			// so a scenario has to be able to tell "writable because it publishes"
			// from "writable because it is the formation writer"
			"formationWriter": utils.IsFormationWriter(),
			"streamSeen":      oplogStreamSeen.Load(),
		})
	})

	// set-device-fields drives the reaction diff: Tags/Nickname must NOT bounce
	// nebula, IP/Blocked must. Pair it with nebulaPid from /status — asserting only
	// "pid unchanged after a tag edit" would also pass if the pre-image were always
	// empty, which silently degrades EVERY topology change to a cache refresh.
	mux.HandleFunc("/set-device-fields", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			DeviceName string
			Tags       []string
			IP         string
			Nickname   string
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			fail(w, err)
			return
		}
		fields := map[string]interface{}{}
		if body.Tags != nil {
			fields["Tags"] = body.Tags
		}
		if body.IP != "" {
			fields["IP"] = body.IP
		}
		if body.Nickname != "" {
			fields["Nickname"] = body.Nickname
		}
		if len(fields) == 0 {
			fail(w, errors.New("no fields given"))
			return
		}
		err := utils.UpdateDevices(map[string]interface{}{"DeviceName": body.DeviceName}, fields)
		if err != nil {
			failStore(w, err)
			return
		}
		writeJSON(w, 200, map[string]interface{}{"ok": true, "status": 200})
	})

	// domain-op exercises kind:"domain" — otherwise the entire domain-registry path
	// is untested end to end. api_tokens is the cheapest domain to set and read back.
	mux.HandleFunc("/domain-op", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Tokens []string }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			fail(w, err)
			return
		}
		tokens := map[string]utils.APITokenConfig{}
		for _, name := range body.Tokens {
			tokens[name] = utils.APITokenConfig{Name: name, Description: "e2e"}
		}
		if err := PublishDomainOp(DomainAPITokens, tokens); err != nil {
			failStore(w, err)
			return
		}
		writeJSON(w, 200, map[string]interface{}{"ok": true, "status": 200})
	})

	// domain-state reads the applied domain back, so a domain op's propagation is
	// observable the way /db makes a table op's propagation observable.
	mux.HandleFunc("/domain-state", func(w http.ResponseWriter, r *http.Request) {
		names := []string{}
		for name := range utils.GetMainConfig().APITokens {
			names = append(names, name)
		}
		sort.Strings(names)
		writeJSON(w, 200, map[string]interface{}{"apiTokens": names})
	})

	mux.HandleFunc("/restart", func(w http.ResponseWriter, r *http.Request) {
		go RestartNebula()
		writeJSON(w, 200, map[string]interface{}{"ok": true})
	})

	mux.HandleFunc("/quit", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]interface{}{"ok": true})
		go func() {
			time.Sleep(200 * time.Millisecond)
			os.Exit(0)
		}()
	})

	srv := &http.Server{Addr: addr, Handler: mux}
	if err := srv.ListenAndServe(); err != nil {
		utils.Error("[E2E] control server died", err)
		os.Exit(1)
	}
	close(quit)
}
