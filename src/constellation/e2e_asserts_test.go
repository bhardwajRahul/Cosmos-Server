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
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
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

func e2eFileHash(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
