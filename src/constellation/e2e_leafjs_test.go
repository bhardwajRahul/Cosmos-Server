//go:build e2e

package constellation

// In-process probe of the exact NATS topology assumption the agent design
// rests on: a JetStream-DISABLED server soliciting a leafnode connection to a
// JetStream-ENABLED hub must transparently proxy its clients' $JS.API
// requests to the hub. Mirrors StartNATS's server.Options shape (users with
// the product's permission set, TLS on the hub's leaf listener, no accounts).

import (
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

func e2eLeafTestUser(name string) *server.User {
	return &server.User{
		Username: name,
		Password: "secret-" + name,
		Permissions: &server.Permissions{
			Publish: &server.SubjectPermission{Allow: []string{
				"cosmos." + name + ".>", "_INBOX.>", "cosmos._global_.>",
				"cosmos.*.deployments.>",
				"$KV.constellation-nodes.>", "$KV.constellation-deployments.>",
				"$JS.API.STREAM.INFO.>", "$JS.API.>",
			}},
			Subscribe: &server.SubjectPermission{Allow: []string{
				"cosmos." + name + ".>", "_INBOX.>", "cosmos._global_.>",
				"$KV.constellation-nodes.>", "$KV.constellation-deployments.>",
				"$JS.API.STREAM.INFO.>", "$JS.API.>",
			}},
		},
	}
}

func TestE2ELeafnodeJetStreamAPI(t *testing.T) {
	const hubIP = "127.0.1.201"
	const leafIP = "127.0.1.202"

	users := []*server.User{e2eLeafTestUser("hubnode"), e2eLeafTestUser("agentnode")}
	leafUsers := []*server.User{
		{Username: "hubnode", Password: "secret-hubnode"},
		{Username: "agentnode", Password: "secret-agentnode"},
	}

	hubOpts := &server.Options{
		Host: hubIP, Port: 4222,
		ServerName: "hubnode",
		JetStream:  true,
		StoreDir:   t.TempDir(),
		// mirrors StartNATS: lifts the default deny of $JS.API/$KV across
		// account-level leaf links (see the comment there)
		JsAccDefaultDomain: map[string]string{"$G": ""},
		Users:              users,
		LeafNode: server.LeafNodeOpts{
			Host: hubIP, Port: 7422, NoAdvertise: true,
			Users: leafUsers,
		},
	}
	hub, err := server.NewServer(hubOpts)
	if err != nil {
		t.Fatal("hub NewServer:", err)
	}
	go hub.Start()
	defer hub.Shutdown()
	if !hub.ReadyForConnections(10 * time.Second) {
		t.Fatal("hub not ready")
	}

	leafRemote, _ := url.Parse(fmt.Sprintf("nats-leaf://agentnode:secret-agentnode@%s:7422", hubIP))
	leafOpts := &server.Options{
		Host: leafIP, Port: 4222,
		ServerName: "agentnode",
		JetStream:  false,
		// both sides of the link enforce the deny — the agent needs it too
		JsAccDefaultDomain: map[string]string{"$G": ""},
		Users:              users,
		LeafNode: server.LeafNodeOpts{
			Remotes: []*server.RemoteLeafOpts{{URLs: []*url.URL{leafRemote}}},
		},
	}
	leaf, err := server.NewServer(leafOpts)
	if err != nil {
		t.Fatal("leaf NewServer:", err)
	}
	go leaf.Start()
	defer leaf.Shutdown()
	if !leaf.ReadyForConnections(10 * time.Second) {
		t.Fatal("leaf not ready")
	}

	// wait for the leaf link to establish
	deadline := time.Now().Add(15 * time.Second)
	for hub.NumLeafNodes() == 0 && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	if hub.NumLeafNodes() == 0 {
		t.Fatal("leafnode link never established")
	}

	// client on the LEAF server, as the product's agent client connects
	nc, err := nats.Connect(fmt.Sprintf("nats://%s:4222", leafIP),
		nats.UserInfo("agentnode", "secret-agentnode"), nats.Timeout(3*time.Second))
	if err != nil {
		t.Fatal("client connect:", err)
	}
	defer nc.Close()

	js, err := nc.JetStream(nats.MaxWait(5 * time.Second))
	if err != nil {
		t.Fatal("JetStream context:", err)
	}

	if _, err := js.AccountInfo(); err != nil {
		t.Fatalf("JS API not reachable through leafnode: %v", err)
	}

	// and the actual product operation: use a KV bucket created hub-side
	hubClient, err := nats.Connect(fmt.Sprintf("nats://%s:4222", hubIP),
		nats.UserInfo("hubnode", "secret-hubnode"), nats.Timeout(3*time.Second))
	if err != nil {
		t.Fatal("hub client connect:", err)
	}
	defer hubClient.Close()
	hubJS, _ := hubClient.JetStream(nats.MaxWait(5 * time.Second))
	if _, err := hubJS.CreateKeyValue(&nats.KeyValueConfig{Bucket: "constellation-nodes", TTL: 10 * time.Second, Storage: nats.MemoryStorage}); err != nil {
		t.Fatal("hub KV create:", err)
	}

	kv, err := js.KeyValue("constellation-nodes")
	if err != nil {
		t.Fatalf("KV not reachable through leafnode: %v", err)
	}
	if _, err := kv.Put("agentnode", []byte("hb")); err != nil {
		t.Fatalf("KV put through leafnode failed: %v", err)
	}
}
