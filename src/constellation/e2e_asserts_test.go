//go:build e2e

package constellation

// Control-plane client and assertion helpers for the Tier-A E2E harness
// (parent side): HTTP access to node control APIs, condition waits with
// last-state reporting, convergence/heartbeat helpers, and failure
// forensics (race/panic log scanning, log preservation).

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/azukaar/cosmos-server/src/utils"
)

var e2eHTTP = &http.Client{Timeout: 20 * time.Second}

func (n *e2eNode) get(path string) (map[string]interface{}, error) {
	resp, err := e2eHTTP.Get(n.controlURL() + path)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out := map[string]interface{}{}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return out, fmt.Errorf("control %s: HTTP %d: %v", path, resp.StatusCode, out["error"])
	}
	return out, nil
}

func (n *e2eNode) post(path string, body interface{}) (map[string]interface{}, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	resp, err := e2eHTTP.Post(n.controlURL()+path, "application/json", strings.NewReader(string(data)))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out := map[string]interface{}{}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return out, fmt.Errorf("control %s: HTTP %d: %v", path, resp.StatusCode, out["error"])
	}
	return out, nil
}

func (c *e2eCluster) waitFor(d time.Duration, desc string, cond func() bool) {
	c.t.Helper()
	c.waitForDetail(d, desc, func() (bool, string) { return cond(), "" })
}

// waitForDetail is waitFor with a condition that also reports its last
// observed state, included in the failure message on timeout.
func (c *e2eCluster) waitForDetail(d time.Duration, desc string, cond func() (bool, string)) {
	c.t.Helper()
	deadline := time.Now().Add(d)
	detail := ""
	for time.Now().Before(deadline) {
		ok, det := cond()
		if ok {
			return
		}
		detail = det
		time.Sleep(time.Second)
	}
	msg := "e2e: timed out waiting for " + desc
	if detail != "" {
		msg += " (last observed: " + detail + ")"
	}
	c.t.Fatal(msg)
}

// preserveLogs copies every node's logs out of the auto-deleted TempDir so a
// failed run can be post-mortemed, and prints each log's tail into the test
// output.
func (c *e2eCluster) preserveLogs() {
	dest := filepath.Join(os.TempDir(), "cosmos-e2e-failures", c.t.Name()+"-"+fmt.Sprint(time.Now().Unix()))
	for name, n := range c.nodes {
		for _, p := range []string{n.logPath, n.configDir + "cosmos.log"} {
			data, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			out := filepath.Join(dest, name+"-"+filepath.Base(p))
			os.MkdirAll(dest, 0755)
			os.WriteFile(out, data, 0644)
			lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
			if len(lines) > 40 {
				lines = lines[len(lines)-40:]
			}
			c.t.Logf("e2e: tail of %s %s:\n%s", name, filepath.Base(p), strings.Join(lines, "\n"))
		}
	}
	c.t.Log("e2e: full node logs preserved in " + dest)
}

// waitConnected waits until the node's NATS client reports connected.
func (c *e2eCluster) waitConnected(name string, d time.Duration) {
	c.t.Helper()
	n := c.node(name)
	c.waitFor(d, name+" NATS client connected", func() bool {
		st, err := n.get("/status")
		return err == nil && st["clientConnected"] == true
	})
}

// dbHash returns the sha256 of the node's database file, "" on error.
func (c *e2eCluster) dbHash(name string) string {
	st, err := c.node(name).get("/db")
	if err != nil {
		return ""
	}
	h, _ := st["hash"].(string)
	return h
}

// oplogConverged reports whether every named node holds the same logical dump
// AND sits at the same (epoch, seq) — matching dumps alone can be a coincidence
// mid-replay, matching positions cannot.
func (c *e2eCluster) oplogConverged(names ...string) (bool, string) {
	wantHash := ""
	wantPos := ""
	detail := ""

	for _, name := range names {
		st, err := c.node(name).get("/oplog")
		if err != nil {
			return false, name + ": " + err.Error()
		}
		epoch, _ := st["epoch"].(float64)
		seq, _ := st["seq"].(float64)
		pos := fmt.Sprintf("e%.0f/%.0f", epoch, seq)
		hash := c.dbHash(name)

		detail += fmt.Sprintf("%s=%s:%.8s ", name, pos, hash)

		if halted, _ := st["halted"].(bool); halted {
			return false, detail + "(" + name + " HALTED: " + fmt.Sprint(st["haltReason"]) + ")"
		}
		if hash == "" {
			return false, detail + "(" + name + " has no dump)"
		}
		if wantHash == "" {
			wantHash, wantPos = hash, pos
			continue
		}
		if hash != wantHash || pos != wantPos {
			return false, detail
		}
	}
	return true, detail
}

// waitStreamReplicas blocks until the op-log stream is both CONFIGURED for the
// wanted replication and actually placed on that many peers.
//
// Any scenario that takes managers away has to wait for this first. The stream
// is created R1 and raised to R3 asynchronously once the third manager is up, so
// a test that kills two of three managers before the scale-up lands is killing
// two nodes that may hold no replica at all: the survivor still has the only
// copy, its publishes still succeed, and a "writes must fail without quorum"
// assertion fails while the code under test is behaving correctly. Worse, which
// single server an R1 stream lands on is arbitrary, so the same test passes or
// fails run to run for reasons unrelated to the change being tested.
func (c *e2eCluster) waitStreamReplicas(d time.Duration, node string, want int) {
	c.t.Helper()
	c.waitForDetail(d, fmt.Sprintf("op-log stream replicated R%d on %s", want, node),
		func() (bool, string) {
			out, err := c.node(node).get("/stream-info")
			if err != nil {
				return false, err.Error()
			}
			exists, _ := out["exists"].(bool)
			replicas, _ := out["replicas"].(float64)
			peers, _ := out["peers"].(float64)
			detail := fmt.Sprintf("exists=%v replicas=%v peers=%v leader=%v err=%v",
				exists, out["replicas"], out["peers"], out["leader"], out["error"])
			return exists && int(replicas) == want && int(peers) >= want, detail
		})
}

// heartbeatNames returns the keys present in the node's view of the
// constellation-nodes KV bucket.
func (c *e2eCluster) heartbeatNames(name string) []string {
	st, err := c.node(name).get("/heartbeats")
	if err != nil {
		return nil
	}
	raw, _ := st["names"].([]interface{})
	names := []string{}
	for _, v := range raw {
		if s, ok := v.(string); ok {
			names = append(names, s)
		}
	}
	return names
}

func containsAll(have []string, want ...string) bool {
	set := map[string]bool{}
	for _, h := range have {
		set[h] = true
	}
	for _, w := range want {
		if !set[w] {
			return false
		}
	}
	return true
}

// scanLogs fails the test if any node log recorded a data race or a panic.
func (c *e2eCluster) scanLogs() {
	for name, n := range c.nodes {
		for _, p := range []string{n.logPath, n.configDir + "cosmos.log"} {
			data, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			s := string(data)
			for _, marker := range []string{"WARNING: DATA RACE", "panic:"} {
				if idx := strings.Index(s, marker); idx != -1 {
					end := idx + 2000
					if end > len(s) {
						end = len(s)
					}
					c.t.Errorf("e2e: node %s recorded %q in %s:\n%s", name, marker, p, s[idx:end])
				}
			}
		}
	}
}

// e2eDumpHash hashes the canonical sorted-JSON logical dump of users+devices.
func e2eDumpHash() string {
	data, err := utils.BuildLogicalDump()
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// requireFreeControlPorts aborts immediately if a previous run's node is still
// holding one of the fixed loopback control ports.
//
// The nodes bind hard-coded addresses, so only one suite can exist on a machine
// at a time. Without this check a leftover process does not announce itself: the
// new node's control server dies at startup with "address already in use", one
// line deep in a child log, and the scenario then spends its full budget timing
// out against a node that never came up. That reads exactly like a product
// failure — a node unreachable while its peers are fine — and cost real time to
// diagnose as an environment problem. Failing here instead turns minutes of
// misleading red into one accurate sentence.
func requireFreeControlPorts(t *testing.T, specs []e2eNodeSpec) {
	t.Helper()
	for _, s := range specs {
		addr := fmt.Sprintf("127.0.1.%d:%d", s.Octet, e2eControlPort)
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			continue // refused: nothing listening, which is what we want
		}
		conn.Close()
		t.Fatalf("e2e: %s is already in use — a node from a previous run is still alive. "+
			"Only one E2E suite can run at a time on this machine. "+
			"Clear it with: pkill -f 'constellation.test'", addr)
	}
}

// reapNebulaStubs kills the fake nebula processes this cluster's nodes spawned.
//
// The stub is a child of the node process, and scenarios kill nodes with SIGKILL
// to simulate a crash — which by definition gives them no chance to reap their
// own children. So every scenario leaks a handful, and they accumulate: 728 were
// found alive on this machine after a day of gate runs, each still pinning its
// scenario's temp directory. They bind no ports and look harmless, which is why
// nobody noticed, but several hundred stray processes are exactly the kind of
// environmental drift that makes a gate's third run behave differently from its
// first — and a three-run gate whose runs are not equivalent is not a gate.
//
// Matched on this cluster's OWN temp root, never on the process name, so a
// concurrent run's stubs (or anything else on the box called nebula) are left
// alone. Runs before the TempDir cleanup, which is registered earlier and so
// fires later.
func (c *e2eCluster) reapNebulaStubs() {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	reaped := 0
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue // not a process directory
		}
		raw, err := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		if err != nil {
			continue // exited between the listing and the read
		}
		cmd := strings.ReplaceAll(string(raw), "\x00", " ")
		if !strings.Contains(cmd, c.root) || !strings.Contains(cmd, "nebula") {
			continue
		}
		if syscall.Kill(pid, syscall.SIGKILL) == nil {
			reaped++
		}
	}
	if reaped > 0 {
		c.t.Logf("e2e: reaped %d leftover nebula stub process(es)", reaped)
	}
}
