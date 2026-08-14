//go:build e2e

package constellation

// Companion experiments to TestE2ELeafnodeJetStreamAPI: which topology
// changes make the hub's JetStream reachable from a client of the agent-side
// server. Investigation evidence, not invariants — subtests report rather
// than fail so the whole matrix always prints.

import (
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

func e2eStartServer(t *testing.T, opts *server.Options) *server.Server {
	t.Helper()
	s, err := server.NewServer(opts)
	if err != nil {
		t.Fatal("NewServer:", err)
	}
	go s.Start()
	t.Cleanup(s.Shutdown)
	if !s.ReadyForConnections(10 * time.Second) {
		t.Fatal("server not ready")
	}
	return s
}

func e2eProbeHubJS(t *testing.T, hubIP, leafIP string, hub *server.Server, mutateLeaf func(*server.Options)) error {
	t.Helper()

	leafRemote, _ := url.Parse(fmt.Sprintf("nats-leaf://agentnode:secret-agentnode@%s:7422", hubIP))
	leafOpts := &server.Options{
		Host: leafIP, Port: 4222,
		ServerName: "agentnode",
		JetStream:  false,
		Users:      []*server.User{e2eLeafTestUser("agentnode")},
		LeafNode: server.LeafNodeOpts{
			Remotes: []*server.RemoteLeafOpts{{URLs: []*url.URL{leafRemote}}},
		},
	}
	if mutateLeaf != nil {
		mutateLeaf(leafOpts)
	}
	e2eStartServer(t, leafOpts)

	deadline := time.Now().Add(10 * time.Second)
	for hub.NumLeafNodes() == 0 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if hub.NumLeafNodes() == 0 {
		return fmt.Errorf("leaf link never established")
	}

	nc, err := nats.Connect(fmt.Sprintf("nats://%s:4222", leafIP),
		nats.UserInfo("agentnode", "secret-agentnode"), nats.Timeout(3*time.Second))
	if err != nil {
		return fmt.Errorf("client connect: %w", err)
	}
	defer nc.Close()

	js, err := nc.JetStream(nats.MaxWait(5 * time.Second))
	if err != nil {
		return fmt.Errorf("JS context: %w", err)
	}
	if _, err := js.AccountInfo(); err != nil {
		return fmt.Errorf("AccountInfo: %w", err)
	}
	kv, err := js.KeyValue("constellation-nodes")
	if err != nil {
		return fmt.Errorf("KV lookup: %w", err)
	}
	if _, err := kv.Put("agentnode", []byte("hb")); err != nil {
		return fmt.Errorf("KV put: %w", err)
	}
	return nil
}

func e2eHubWithKV(t *testing.T, hubIP string) *server.Server {
	t.Helper()
	hub := e2eStartServer(t, &server.Options{
		Host: hubIP, Port: 4222,
		ServerName: "hubnode",
		JetStream:  true,
		StoreDir:   t.TempDir(),
		Users:      []*server.User{e2eLeafTestUser("hubnode"), e2eLeafTestUser("agentnode")},
		LeafNode: server.LeafNodeOpts{
			Host: hubIP, Port: 7422, NoAdvertise: true,
			Users: []*server.User{
				{Username: "hubnode", Password: "secret-hubnode"},
				{Username: "agentnode", Password: "secret-agentnode"},
			},
		},
	})
	nc, err := nats.Connect(fmt.Sprintf("nats://%s:4222", hubIP),
		nats.UserInfo("hubnode", "secret-hubnode"), nats.Timeout(3*time.Second))
	if err != nil {
		t.Fatal("hub client:", err)
	}
	t.Cleanup(nc.Close)
	js, _ := nc.JetStream(nats.MaxWait(5 * time.Second))
	if _, err := js.CreateKeyValue(&nats.KeyValueConfig{Bucket: "constellation-nodes", TTL: 10 * time.Second, Storage: nats.MemoryStorage}); err != nil {
		t.Fatal("hub KV create:", err)
	}
	return hub
}

func TestE2ELeafnodeJetStreamVariants(t *testing.T) {
	t.Run("agent client connects directly to hub", func(t *testing.T) {
		hub := e2eHubWithKV(t, "127.0.1.211")
		_ = hub
		nc, err := nats.Connect("nats://127.0.1.211:4222",
			nats.UserInfo("agentnode", "secret-agentnode"), nats.Timeout(3*time.Second))
		if err != nil {
			t.Log("RESULT: FAIL —", err)
			return
		}
		defer nc.Close()
		js, _ := nc.JetStream(nats.MaxWait(5 * time.Second))
		if _, err := js.AccountInfo(); err != nil {
			t.Log("RESULT: FAIL —", err)
			return
		}
		kv, err := js.KeyValue("constellation-nodes")
		if err == nil {
			_, err = kv.Put("agentnode", []byte("hb"))
		}
		if err != nil {
			t.Log("RESULT: FAIL —", err)
			return
		}
		t.Log("RESULT: OK — direct client to hub reaches JS (baseline)")
	})

	t.Run("leaf server with own JetStream and domain on hub", func(t *testing.T) {
		hubIP, leafIP := "127.0.1.213", "127.0.1.214"
		hub := e2eStartServer(t, &server.Options{
			Host: hubIP, Port: 4222, ServerName: "hubnode",
			JetStream: true, JetStreamDomain: "hub", StoreDir: t.TempDir(),
			Users: []*server.User{e2eLeafTestUser("hubnode"), e2eLeafTestUser("agentnode")},
			LeafNode: server.LeafNodeOpts{Host: hubIP, Port: 7422, NoAdvertise: true,
				Users: []*server.User{
					{Username: "hubnode", Password: "secret-hubnode"},
					{Username: "agentnode", Password: "secret-agentnode"},
				}},
		})
		ncHub, err := nats.Connect(fmt.Sprintf("nats://%s:4222", hubIP),
			nats.UserInfo("hubnode", "secret-hubnode"), nats.Timeout(3*time.Second))
		if err != nil {
			t.Log("RESULT: FAIL — hub client:", err)
			return
		}
		defer ncHub.Close()
		jsHub, _ := ncHub.JetStream(nats.MaxWait(5 * time.Second))
		if _, err := jsHub.CreateKeyValue(&nats.KeyValueConfig{Bucket: "constellation-nodes", TTL: 10 * time.Second, Storage: nats.MemoryStorage}); err != nil {
			t.Log("RESULT: FAIL — hub KV create:", err)
			return
		}

		err = e2eProbeHubJSWithDomain(t, hubIP, leafIP, hub, "hub")
		if err != nil {
			t.Log("RESULT: FAIL —", err)
			return
		}
		t.Log("RESULT: OK — leaf with own JS + client pinned to hub domain reaches hub JS")
	})
}

// e2eProbeHubJSWithDomain: leaf server runs its OWN JetStream (separate
// domain), client asks explicitly for the hub's domain.
func e2eProbeHubJSWithDomain(t *testing.T, hubIP, leafIP string, hub *server.Server, hubDomain string) error {
	t.Helper()
	leafRemote, _ := url.Parse(fmt.Sprintf("nats-leaf://agentnode:secret-agentnode@%s:7422", hubIP))
	e2eStartServer(t, &server.Options{
		Host: leafIP, Port: 4222, ServerName: "agentnode",
		JetStream: true, JetStreamDomain: "leaf-agentnode", StoreDir: t.TempDir(),
		Users: []*server.User{e2eLeafTestUser("agentnode")},
		LeafNode: server.LeafNodeOpts{
			Remotes: []*server.RemoteLeafOpts{{URLs: []*url.URL{leafRemote}}},
		},
	})

	deadline := time.Now().Add(10 * time.Second)
	for hub.NumLeafNodes() == 0 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if hub.NumLeafNodes() == 0 {
		return fmt.Errorf("leaf link never established")
	}

	nc, err := nats.Connect(fmt.Sprintf("nats://%s:4222", leafIP),
		nats.UserInfo("agentnode", "secret-agentnode"), nats.Timeout(3*time.Second))
	if err != nil {
		return fmt.Errorf("client connect: %w", err)
	}
	defer nc.Close()

	js, err := nc.JetStream(nats.MaxWait(5*time.Second), nats.Domain(hubDomain))
	if err != nil {
		return fmt.Errorf("JS context: %w", err)
	}
	if _, err := js.AccountInfo(); err != nil {
		return fmt.Errorf("AccountInfo(domain): %w", err)
	}
	kv, err := js.KeyValue("constellation-nodes")
	if err != nil {
		return fmt.Errorf("KV lookup(domain): %w", err)
	}
	if _, err := kv.Put("agentnode", []byte("hb")); err != nil {
		return fmt.Errorf("KV put(domain): %w", err)
	}
	return nil
}
