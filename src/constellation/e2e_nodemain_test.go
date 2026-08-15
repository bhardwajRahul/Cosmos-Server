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

	// control API first, so the parent can observe startup states too
	quit := make(chan struct{})
	go e2eControlServer(os.Getenv("COSMOS_E2E_CONTROL_ADDR"), quit)

	Init()

	<-quit
}

func e2eControlServer(addr string, quit chan struct{}) {
	mux := http.NewServeMux()

	writeJSON := func(w http.ResponseWriter, code int, v interface{}) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(v)
	}
	fail := func(w http.ResponseWriter, err error) {
		writeJSON(w, 500, map[string]interface{}{"error": err.Error()})
	}

	// fast, no network calls — safe to poll aggressively
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		name, _ := GetCurrentDeviceName()
		leafs := -1
		routes := -1
		if ns != nil {
			leafs = ns.NumLeafNodes()
			routes = ns.NumRoutes()
		}
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
		if ns == nil {
			fail(w, errNoJS)
			return
		}
		routez, errR := ns.Routez(nil)
		leafz, errL := ns.Leafz(nil)
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
			"hash":    e2eFileHash(utils.CONFIGFOLDER + "database"),
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

	mux.HandleFunc("/sync-push", func(w http.ResponseWriter, r *http.Request) {
		SendNewDBSyncMessage()
		writeJSON(w, 200, map[string]interface{}{"ok": true})
	})

	mux.HandleFunc("/sync-request", func(w http.ResponseWriter, r *http.Request) {
		SendRequestSyncMessage()
		writeJSON(w, 200, map[string]interface{}{"ok": true})
	})

	// edit-device mutates the local device DB the way the edit API does,
	// bumping the database file mtime that drives sync conflict resolution
	mux.HandleFunc("/edit-device", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ DeviceName, Nickname string }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			fail(w, err)
			return
		}
		c, closeDb, err := utils.GetEmbeddedCollection(utils.GetRootAppId(), "devices")
		if err != nil {
			fail(w, err)
			return
		}
		_, err = c.UpdateOne(nil,
			map[string]interface{}{"DeviceName": body.DeviceName},
			map[string]interface{}{"$set": map[string]interface{}{"Nickname": body.Nickname}})
		closeDb()
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]interface{}{"ok": true})
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
		c, closeDb, err := utils.GetEmbeddedCollection(utils.GetRootAppId(), "devices")
		if err != nil {
			fail(w, err)
			return
		}
		_, err = c.UpdateOne(nil,
			map[string]interface{}{"DeviceName": body.DeviceName},
			map[string]interface{}{"$set": map[string]interface{}{"Blocked": body.Blocked}})
		closeDb()
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]interface{}{"ok": true})
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
