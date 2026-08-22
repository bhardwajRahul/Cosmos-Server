package constellation

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/azukaar/cosmos-server/src/utils"
)

// fakeKV implements nats.KeyValue with a controllable Keys() result;
// every other method panics via the embedded nil interface.
type fakeKV struct {
	nats.KeyValue
	keys []string
	err  error
}

func (f fakeKV) Keys(opts ...nats.WatchOpt) ([]string, error) { return f.keys, f.err }

// fakeJS records bucket delete/create calls; every other method panics.
type fakeJS struct {
	nats.JetStreamContext
	deleted []string
	created []string
}

func (f *fakeJS) DeleteKeyValue(bucket string) error {
	f.deleted = append(f.deleted, bucket)
	return nil
}

func (f *fakeJS) CreateKeyValue(cfg *nats.KeyValueConfig) (nats.KeyValue, error) {
	f.created = append(f.created, cfg.Bucket)
	return fakeKV{}, nil
}

func seedDeviceCache(t *testing.T, devices ...utils.ConstellationDevice) {
	t.Helper()
	newNames := map[string]string{}
	newDevices := map[string]utils.ConstellationDevice{}
	for _, d := range devices {
		newNames[d.DeviceName] = d.IP
		newDevices[d.DeviceName] = d
	}
	deviceCacheMux.Lock()
	cachedCurrentDevice = nil
	CachedDeviceNames = newNames
	CachedDevices = newDevices
	deviceCacheMux.Unlock()
}

func TestUnitDesignatedKVCreator(t *testing.T) {
	setupTestEnv(t, func(cfg *utils.Config) {
		cfg.ConstellationConfig.ThisDeviceName = "node-b"
	})

	manager := func(name string) utils.ConstellationDevice {
		return utils.ConstellationDevice{DeviceName: name, IP: "192.168.201.1", CosmosNode: 2}
	}

	t.Run("self is lowest", func(t *testing.T) {
		seedDeviceCache(t, manager("node-b"), manager("node-c"), manager("node-d"))
		creator, isSelf := designatedKVCreator()
		if creator != "node-b" || !isSelf {
			t.Errorf("designatedKVCreator() = (%q, %v), want (node-b, true)", creator, isSelf)
		}
	})

	t.Run("another manager is lowest", func(t *testing.T) {
		seedDeviceCache(t, manager("node-a"), manager("node-b"), manager("node-c"))
		creator, isSelf := designatedKVCreator()
		if creator != "node-a" || isSelf {
			t.Errorf("designatedKVCreator() = (%q, %v), want (node-a, false)", creator, isSelf)
		}
	})

	t.Run("agents are not candidates", func(t *testing.T) {
		agent := utils.ConstellationDevice{DeviceName: "node-a", IP: "192.168.201.9", CosmosNode: 1}
		seedDeviceCache(t, agent, manager("node-b"))
		creator, isSelf := designatedKVCreator()
		if creator != "node-b" || !isSelf {
			t.Errorf("designatedKVCreator() = (%q, %v), want (node-b, true)", creator, isSelf)
		}
	})
}

func TestUnitDesignatedKVCreatorUnknownSelf(t *testing.T) {
	// no ThisDeviceName, no nebula.yml: cannot identify ourselves → claim creatorship
	setupTestEnv(t, nil)
	creator, isSelf := designatedKVCreator()
	if creator != "" || !isSelf {
		t.Errorf("designatedKVCreator() = (%q, %v), want (\"\", true)", creator, isSelf)
	}
}

func TestUnitCheckNodesKVDivergenceCounting(t *testing.T) {
	setupTestEnv(t, func(cfg *utils.Config) {
		cfg.ConstellationConfig.ThisDeviceName = "node-b"
	})
	// node-a is the designated creator, so this node never enters the cure path
	seedDeviceCache(t,
		utils.ConstellationDevice{DeviceName: "node-a", IP: "192.168.201.1", CosmosNode: 2},
		utils.ConstellationDevice{DeviceName: "node-b", IP: "192.168.201.2", CosmosNode: 2},
	)

	clean := fakeKV{keys: []string{"node_a", "node_b"}}
	diverged := fakeKV{keys: []string{"node_a", "node_b", "node_a"}}
	failing := fakeKV{err: errors.New("transport error")}

	consecutive := 0

	checkNodesKVDivergence(diverged, &consecutive)
	if consecutive != 1 {
		t.Fatalf("after 1 diverged check, consecutive = %d, want 1", consecutive)
	}

	checkNodesKVDivergence(clean, &consecutive)
	if consecutive != 0 {
		t.Fatalf("clean check should reset, consecutive = %d, want 0", consecutive)
	}

	checkNodesKVDivergence(diverged, &consecutive)
	checkNodesKVDivergence(failing, &consecutive)
	if consecutive != 0 {
		t.Fatalf("Keys() error should reset, consecutive = %d, want 0", consecutive)
	}

	// 3 consecutive positives as NON-creator: counter re-arms, no cure attempted
	// (js is nil here — touching it would panic, which is part of the assertion)
	checkNodesKVDivergence(diverged, &consecutive)
	checkNodesKVDivergence(diverged, &consecutive)
	checkNodesKVDivergence(diverged, &consecutive)
	if consecutive != 0 {
		t.Fatalf("after 3 diverged checks, consecutive = %d, want 0 (re-armed)", consecutive)
	}
}

func TestUnitNodesKVDivergenceCure(t *testing.T) {
	setupTestEnv(t, func(cfg *utils.Config) {
		cfg.ConstellationConfig.ThisDeviceName = "node-a"
	})
	// this node is the designated creator
	seedDeviceCache(t,
		utils.ConstellationDevice{DeviceName: "node-a", IP: "192.168.201.1", CosmosNode: 2},
		utils.ConstellationDevice{DeviceName: "node-b", IP: "192.168.201.2", CosmosNode: 2},
	)

	prevJS := js
	prevCure := lastNodesKVCure
	fake := &fakeJS{}
	js = fake
	t.Cleanup(func() {
		js = prevJS
		lastNodesKVCure = prevCure
	})

	diverged := fakeKV{keys: []string{"node_a", "node_a"}}

	t.Run("cooldown blocks re-cure", func(t *testing.T) {
		lastNodesKVCure = time.Now()
		consecutive := 2
		checkNodesKVDivergence(diverged, &consecutive)
		if len(fake.deleted) != 0 || len(fake.created) != 0 {
			t.Errorf("cure ran within cooldown: deleted=%v created=%v", fake.deleted, fake.created)
		}
		if consecutive != 0 {
			t.Errorf("consecutive = %d, want 0 (re-armed)", consecutive)
		}
	})

	t.Run("cure runs after cooldown", func(t *testing.T) {
		lastNodesKVCure = time.Now().Add(-nodesKVCureCooldown - time.Minute)
		before := lastNodesKVCure
		consecutive := 2
		checkNodesKVDivergence(diverged, &consecutive)
		if len(fake.deleted) != 1 || fake.deleted[0] != "constellation-nodes" {
			t.Errorf("deleted = %v, want [constellation-nodes]", fake.deleted)
		}
		if len(fake.created) != 1 || fake.created[0] != "constellation-nodes" {
			t.Errorf("created = %v, want [constellation-nodes]", fake.created)
		}
		if !lastNodesKVCure.After(before) {
			t.Error("lastNodesKVCure was not updated by the cure")
		}
	})
}

func TestUnitNatsServerWatchdogExits(t *testing.T) {
	setupTestEnv(t, nil)

	run := func(t *testing.T) {
		t.Helper()
		done := make(chan struct{})
		go func() {
			natsServerWatchdog()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("natsServerWatchdog did not exit")
		}
	}

	t.Run("exits when nebula is down", func(t *testing.T) {
		NebulaStarted.Store(false)
		NATSStarted.Store(false)
		run(t)
	})

	t.Run("exits when NATS is already up", func(t *testing.T) {
		NebulaStarted.Store(true)
		NATSStarted.Store(true)
		run(t)
	})

	t.Run("single-flight", func(t *testing.T) {
		// claim the slot: a second watchdog must return immediately even
		// though the liveness conditions would keep it looping
		NebulaStarted.Store(true)
		NATSStarted.Store(false)
		if !atomic.CompareAndSwapInt32(&natsServerWatchdogRunning, 0, 1) {
			t.Fatal("watchdog flag unexpectedly claimed")
		}
		defer atomic.StoreInt32(&natsServerWatchdogRunning, 0)
		run(t)
	})
}

func TestUnitRefreshDeviceCacheInvalidatesStaleCurrentDevice(t *testing.T) {
	setupTestEnv(t, nil)
	writeNebulaYML(t, map[string]interface{}{
		"cstln_device_name": "fresh-node",
		"cstln_ip":          "192.168.201.5/24",
		"cstln_api_key":     "test-key",
	})

	// stale cached current device that must NOT survive the refresh
	stale := utils.ConstellationDevice{DeviceName: "fresh-node", IP: "10.0.0.99/24", APIKey: "stale-key"}
	deviceCacheMux.Lock()
	cachedCurrentDevice = &stale
	deviceCacheMux.Unlock()

	// DB is empty, so the current device is re-derived — from nebula.yml, not the stale cache
	refreshDeviceCache()

	devices, _ := deviceCacheSnapshot()
	got, exists := devices["fresh-node"]
	if !exists {
		t.Fatal("current device missing from refreshed cache")
	}
	if got.IP != "192.168.201.5/24" {
		t.Errorf("cached IP = %q, want fresh 192.168.201.5/24 (stale cache leaked through)", got.IP)
	}
}

