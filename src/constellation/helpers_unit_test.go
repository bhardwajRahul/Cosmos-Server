package constellation

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/azukaar/cosmos-server/src/utils"
)

func TestUnitMatchesDomain(t *testing.T) {
	tests := []struct {
		name     string
		qName    string
		hostname string
		want     bool
	}{
		{"exact match", "myhost.com.", "myhost.com", true},
		{"subdomain match", "a.myhost.com.", "myhost.com", true},
		{"deep subdomain match", "a.b.myhost.com.", "myhost.com", true},
		{"regression: suffix without label boundary", "evilmyhost.com.", "myhost.com", false},
		{"non-match", "other.com.", "myhost.com", false},
		{"missing trailing dot", "myhost.com", "myhost.com", false},
		{"empty qName", "", "myhost.com", false},
		{"empty hostname", "myhost.com.", "", false},
		{"both empty", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matchesDomain(tt.qName, tt.hostname); got != tt.want {
				t.Errorf("matchesDomain(%q, %q) = %v, want %v", tt.qName, tt.hostname, got, tt.want)
			}
		})
	}
}

func TestUnitIsDomain(t *testing.T) {
	tests := []struct {
		domain string
		want   bool
	}{
		{"ads.example.com", true},
		{"example.com", true},
		{"0.0.0.0", false},
		{"localhost", false},
		{"", false},
		{"bad domain.com", false},
		{"weird!.com", false},
		{"path/to.com", false},
	}
	for _, tt := range tests {
		if got := isDomain(tt.domain); got != tt.want {
			t.Errorf("isDomain(%q) = %v, want %v", tt.domain, got, tt.want)
		}
	}
}

func TestUnitLoadRawBlockList(t *testing.T) {
	raw := strings.Join([]string{
		"# comment line",
		"",
		"0.0.0.0 ads.example.com",
		"bare-domain.org",
		"garbage line with words",
		"tracker.net 0.0.0.0",
		"0.0.0.0",
		"# another.commented.com",
	}, "\n")

	blacklist := map[string]bool{}
	loadRawBlockList(blacklist, raw)

	want := map[string]bool{
		"ads.example.com": true,
		"bare-domain.org": true,
		"tracker.net":     true,
	}
	if len(blacklist) != len(want) {
		t.Errorf("blacklist has %d entries, want %d: %v", len(blacklist), len(want), blacklist)
	}
	for domain := range want {
		if !blacklist[domain] {
			t.Errorf("expected %q in blacklist", domain)
		}
	}
	for _, bad := range []string{"0.0.0.0", "# comment line", "garbage", "another.commented.com"} {
		if blacklist[bad] {
			t.Errorf("unexpected %q in blacklist", bad)
		}
	}
}

func TestUnitSanitizeNATSUsername(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"my device", "my_device"},
		{"a.b-c:d/e\\f", "a_b_c_d_e_f"},
		{"clean_name", "clean_name"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := sanitizeNATSUsername(tt.in); got != tt.want {
			t.Errorf("sanitizeNATSUsername(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestUnitRedactSecret(t *testing.T) {
	if got := redactSecret(""); got != "<empty>" {
		t.Errorf("redactSecret(\"\") = %q, want \"<empty>\"", got)
	}

	// short secrets must not leak any prefix
	short := "abcd"
	got := redactSecret(short)
	if got != "****(len 4)" {
		t.Errorf("redactSecret(%q) = %q, want \"****(len 4)\"", short, got)
	}
	if strings.Contains(got, "a") {
		t.Errorf("short secret prefix leaked in %q", got)
	}

	// long secrets show a 4-char prefix and length only
	long := "supersecretvalue"
	got = redactSecret(long)
	if got != "supe****(len 16)" {
		t.Errorf("redactSecret(%q) = %q, want \"supe****(len 16)\"", long, got)
	}
	if strings.Contains(got, long[4:]) {
		t.Errorf("secret suffix leaked in %q", got)
	}
}

func TestUnitTruncateLog(t *testing.T) {
	short := "hello"
	if got := truncateLog(short); got != short {
		t.Errorf("truncateLog(%q) = %q, want unchanged", short, got)
	}

	exact := strings.Repeat("x", 100)
	if got := truncateLog(exact); got != exact {
		t.Errorf("truncateLog of 100 chars should be unchanged, got %q", got)
	}

	long := strings.Repeat("y", 150)
	got := truncateLog(long)
	if got != strings.Repeat("y", 100)+"..." {
		t.Errorf("truncateLog of 150 chars = %q, want 100 chars + ellipsis", got)
	}
}

func TestUnitCleanIp(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"192.168.201.1/24", "192.168.201.1"},
		{"192.168.201.1", "192.168.201.1"},
		{"10.0.0.5/16", "10.0.0.5"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := cleanIp(tt.in); got != tt.want {
			t.Errorf("cleanIp(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestUnitGetCertPrefixLength(t *testing.T) {
	tests := []struct {
		name    string
		ipRange string
		want    string
	}{
		{"empty defaults to 24", "", "24"},
		{"valid /16 range", "192.168.0.0/16", "16"},
		{"invalid range defaults to 24", "not-a-cidr", "24"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setupTestEnv(t, func(cfg *utils.Config) {
				cfg.ConstellationConfig.IPRange = tt.ipRange
			})
			if got := getCertPrefixLength(); got != tt.want {
				t.Errorf("getCertPrefixLength() with IPRange %q = %q, want %q", tt.ipRange, got, tt.want)
			}
		})
	}
}

func TestUnitConnectToExistingValid(t *testing.T) {
	setupTestEnv(t, nil)

	yamlBody := []byte(`
cstln_device_name: my-device
cstln_public_hostname: vpn.example.com
cstln_ip_range: 10.10.0.0/16
cstln_cosmos_node: 1
`)
	cfg, err := ConnectToExisting(yamlBody, utils.GetMainConfig())
	if err != nil {
		t.Fatal("ConnectToExisting:", err)
	}
	if cfg.ConstellationConfig.ThisDeviceName != "my-device" {
		t.Errorf("ThisDeviceName = %q, want \"my-device\"", cfg.ConstellationConfig.ThisDeviceName)
	}
	if cfg.ConstellationConfig.ConstellationHostname != "vpn.example.com" {
		t.Errorf("ConstellationHostname = %q, want \"vpn.example.com\"", cfg.ConstellationConfig.ConstellationHostname)
	}
	if cfg.ConstellationConfig.IPRange != "10.10.0.0/16" {
		t.Errorf("IPRange = %q, want \"10.10.0.0/16\"", cfg.ConstellationConfig.IPRange)
	}
	if !cfg.AgentMode {
		t.Error("AgentMode = false, want true (cstln_cosmos_node: 1)")
	}
	if !cfg.ConstellationConfig.Enabled {
		t.Error("Enabled = false, want true")
	}
}

// A joining node must start at the constellation's real epoch, not the default 1.
// Asking at 1 is outranked by every stale responder — including managers revived
// from an epoch a force-reform abandoned — and the snapshot responder can only
// silence peers BEHIND the asker, so a replacement could be captured at a dead
// epoch. Adoption is forward-only: a lower epoch in an enrollment bundle must
// never walk a healthy node backwards into a log its cluster has left.
func TestUnitConnectToExistingAdoptsEpochForwardOnly(t *testing.T) {
	setupTestEnv(t, nil)

	enroll := func(epoch string) {
		t.Helper()
		if _, err := ConnectToExisting([]byte("cstln_device_name: d\ncstln_oplog_epoch: "+epoch+"\n"),
			utils.GetMainConfig()); err != nil {
			t.Fatal("ConnectToExisting:", err)
		}
	}

	enroll("4")
	if got := utils.GetOplogEpoch(); got != 4 {
		t.Fatalf("epoch = %d, want 4 — a joiner asking at the default 1 is outranked by stale peers", got)
	}
	// it has the epoch's name, not its contents: it must re-materialize before writing
	if utils.IsOplogBootstrapped() {
		t.Fatal("joiner claims materialized state for an epoch it has never seen")
	}

	if err := utils.MarkOplogBootstrapped(4); err != nil {
		t.Fatal("MarkOplogBootstrapped:", err)
	}

	// a stale bundle must not move it back, nor discard the state it holds
	enroll("2")
	if got := utils.GetOplogEpoch(); got != 4 {
		t.Fatalf("epoch = %d after a stale enrollment bundle, want 4 — adopting backwards "+
			"walks this node into a log the cluster has abandoned", got)
	}
	if !utils.IsOplogBootstrapped() {
		t.Fatal("a stale enrollment bundle cleared the bootstrapped marker")
	}

	// an equal epoch is a no-op rather than a pointless re-snapshot
	enroll("4")
	if !utils.IsOplogBootstrapped() {
		t.Fatal("re-enrolling at the same epoch forced a needless re-materialization")
	}

	// and a bundle without the key at all still works (older manager, mixed versions)
	enroll4Missing := func() {
		if _, err := ConnectToExisting([]byte("cstln_device_name: d\n"), utils.GetMainConfig()); err != nil {
			t.Fatal("ConnectToExisting without an epoch key:", err)
		}
	}
	enroll4Missing()
	if got := utils.GetOplogEpoch(); got != 4 {
		t.Fatalf("epoch = %d after an enrollment bundle with no epoch key, want 4", got)
	}
}

// stubSoftRestartHooks fills in the restart callbacks main() installs: they are
// nil in a test binary, and ResetNebula ends in SoftRestartServer calling them
// from a goroutine.
func stubSoftRestartHooks(t *testing.T) {
	t.Helper()

	prev := struct {
		waitForAllJobs       func()
		restartConstellation func()
		initRemoteStorage    func()
		initBackups          func()
		initSnapRAIDConfig   func()
		restartCRON          func()
	}{
		utils.WaitForAllJobs, utils.RestartConstellation, utils.InitRemoteStorage,
		utils.InitBackups, utils.InitSnapRAIDConfig, utils.RestartCRON,
	}
	// SoftRestartServer reads these from a goroutine it does not wait for, so
	// restore only after the last one fires; if it never does, leave the inert
	// noops installed rather than race.
	done := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Log("stubSoftRestartHooks: SoftRestartServer never ran, leaving the stubs installed")
			return
		}
		utils.WaitForAllJobs = prev.waitForAllJobs
		utils.RestartConstellation = prev.restartConstellation
		utils.InitRemoteStorage = prev.initRemoteStorage
		utils.InitBackups = prev.initBackups
		utils.InitSnapRAIDConfig = prev.initSnapRAIDConfig
		utils.RestartCRON = prev.restartCRON
	})

	noop := func() {}
	utils.WaitForAllJobs = noop
	utils.RestartConstellation = noop
	utils.InitRemoteStorage = noop
	utils.InitBackups = noop
	utils.InitSnapRAIDConfig = noop
	// last of the six, so this is the goroutine signing off
	utils.RestartCRON = func() { once.Do(func() { close(done) }) }
}

// A reset empties the device table, so the op-log position and bootstrapped
// marker must go with it: left behind, the epoch-tied marker makes the next
// constellation created on this node read-only from the moment it is enabled.
func TestUnitResetNebulaClearsOplogState(t *testing.T) {
	setupTestEnv(t, nil)
	stubSoftRestartHooks(t)

	epoch := utils.GetOplogEpoch()
	if err := utils.MarkOplogBootstrapped(epoch); err != nil {
		t.Fatal("MarkOplogBootstrapped:", err)
	}
	if err := utils.CommitOplogSeq(42); err != nil {
		t.Fatal("CommitOplogSeq:", err)
	}
	oplogStreamSeen.Store(true)

	// precondition: standalone with a live constellation is the writable case,
	// and it is read-only purely because of the marker
	if got := oplogWriteMode(); got != oplogReadOnly {
		t.Fatalf("oplogWriteMode() = %v before the reset, want oplogReadOnly — "+
			"the test is no longer reproducing the state it was written for", got)
	}

	if err := ResetNebula(); err != nil {
		t.Fatal("ResetNebula:", err)
	}

	if utils.IsOplogBootstrapped() {
		t.Error("the reset left the bootstrapped marker behind: this node claims " +
			"state it materialized from a log whose devices it just deleted")
	}
	if got := utils.GetLastAppliedSeq(); got != 0 {
		t.Errorf("last_applied_seq = %d after a reset, want 0 — a new log at the same "+
			"epoch restarts at seq 1, and oplogAttach subscribes from last+1, so a stale "+
			"position means this node never sees its own ops come back", got)
	}
	if oplogStreamSeen.Load() {
		t.Error("the reset left oplogStreamSeen set, which disables the formation " +
			"direct-write path — the only writable path before a second manager enrolls")
	}
	if utils.IsFormationWriter() {
		t.Error("the reset left a formation licence behind")
	}
	// keeping the epoch across a reset is deliberate (see ResetNebula)
	if got := utils.GetOplogEpoch(); got != epoch {
		t.Errorf("oplog epoch = %d after a reset, want %d unchanged", got, epoch)
	}

	// creating a constellation here again must be writable
	config := utils.ReadConfigFromFile()
	config.ConstellationConfig.Enabled = true
	utils.SetBaseMainConfig(config)
	if err := utils.SetFormationWriter(utils.GetOplogEpoch()); err != nil {
		t.Fatal("SetFormationWriter:", err)
	}

	if got := oplogWriteMode(); got == oplogReadOnly {
		t.Fatal("a constellation created after a reset is read-only: every device " +
			"and user write on a brand-new install fails with ErrReadOnly")
	}
}

func TestUnitConnectToExistingWrongTypes(t *testing.T) {
	// regression: wrong-typed fields must return an error, not panic
	tests := []struct {
		name string
		yaml string
	}{
		{"device name is int", "cstln_device_name: 123"},
		{"public hostname is int", "cstln_device_name: ok\ncstln_public_hostname: 42"},
		{"licence is int", "cstln_device_name: ok\ncstln_server_licence: 42"},
		{"cosmos node is string", "cstln_device_name: ok\ncstln_cosmos_node: notanint"},
		{"ip range is int", "cstln_device_name: ok\ncstln_ip_range: 99"},
		{"missing device name", "cstln_public_hostname: vpn.example.com"},
		{"invalid yaml", ":\n  - ["},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setupTestEnv(t, nil)
			if _, err := ConnectToExisting([]byte(tt.yaml), utils.GetMainConfig()); err == nil {
				t.Error("expected error, got nil")
			}
		})
	}
}
