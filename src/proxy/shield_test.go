package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/azukaar/cosmos-server/src/constellation"
	"github.com/azukaar/cosmos-server/src/utils"
)

const (
	testSelfIP     = "10.42.0.1"
	testSelfAPIKey = "self-api-key"
	testClientIP   = "203.0.113.7"
)

// registers this node in the overlay and in the device store, so its own API key authenticates against its own IP
func setupConstellationDevice(t *testing.T) {
	t.Helper()

	prevFolder := utils.CONFIGFOLDER
	utils.CONFIGFOLDER = t.TempDir() + "/"
	if err := utils.InitStore(); err != nil {
		t.Fatal("InitStore:", err)
	}
	if err := utils.CreateDevice(utils.ConstellationDevice{
		DeviceName: "self",
		IP:         testSelfIP,
		APIKey:     testSelfAPIKey,
	}); err != nil {
		t.Fatal("CreateDevice:", err)
	}

	cfg := utils.Config{}
	cfg.ConstellationConfig.Enabled = true
	cfg.ConstellationConfig.ThisDeviceName = "self"
	utils.LoadBaseMainConfig(cfg)

	constellation.NebulaStarted.Store(true)
	constellation.CachedDeviceNames = map[string]string{"self": testSelfIP}

	t.Cleanup(func() {
		constellation.NebulaStarted.Store(false)
		constellation.CachedDeviceNames = map[string]string{}
		utils.CloseStore()
		utils.CONFIGFOLDER = prevFolder
	})
}

func clientIDOf(t *testing.T, remoteAddr string, apiKey string, forwardedFor string) string {
	t.Helper()

	r := httptest.NewRequest("GET", "http://tunnel.example/", nil)
	r.RemoteAddr = remoteAddr
	if apiKey != "" {
		r.Header.Set("x-cstln-auth", apiKey)
	}
	if forwardedFor != "" {
		r.Header.Set("X-Forwarded-For", forwardedFor)
	}
	return GetClientID(r, utils.ProxyRouteConfig{})
}

// the self-token case: a valid token with no forwarded address used to yield "", silently disabling stickiness
func TestGetClientIDFallsBackWhenForwardedForIsEmpty(t *testing.T) {
	setupConstellationDevice(t)

	if id := clientIDOf(t, testSelfIP+":4242", testSelfAPIKey, ""); id != testSelfIP {
		t.Fatalf("empty client ID: got %q, want %q", id, testSelfIP)
	}
	if id := clientIDOf(t, testSelfIP+":4242", testSelfAPIKey, "   "); id != testSelfIP {
		t.Fatalf("blank forwarded address: got %q, want %q", id, testSelfIP)
	}
}

func TestGetClientIDTrustsForwardedForFromConstellationPeer(t *testing.T) {
	setupConstellationDevice(t)

	if id := clientIDOf(t, testSelfIP+":4242", testSelfAPIKey, testClientIP); id != testClientIP {
		t.Fatalf("authenticated peer's forwarded client address ignored: got %q, want %q", id, testClientIP)
	}
}

func TestGetClientIDIgnoresForwardedForWithoutToken(t *testing.T) {
	setupConstellationDevice(t)

	if id := clientIDOf(t, testSelfIP+":4242", "", testClientIP); id != testSelfIP {
		t.Fatalf("unauthenticated overlay client spoofed its address: got %q, want %q", id, testSelfIP)
	}
	if id := clientIDOf(t, testSelfIP+":4242", "wrong-key", testClientIP); id != testSelfIP {
		t.Fatalf("bad token accepted: got %q, want %q", id, testSelfIP)
	}
}

func TestNewProxyStripsConstellationTokenFromBackend(t *testing.T) {
	utils.LoadBaseMainConfig(utils.Config{})

	seen := make(chan http.Header, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Clone()
	}))
	defer backend.Close()

	route := utils.ProxyRouteConfig{Name: "backend-route", Mode: "PROXY", Target: backend.URL}
	proxy, err := NewProxy(backend.URL, false, false, route)
	if err != nil {
		t.Fatal("NewProxy:", err)
	}

	r := httptest.NewRequest("GET", "http://app.example/", nil)
	r.RemoteAddr = testSelfIP + ":4242"
	r.Header.Set("x-cstln-auth", testSelfAPIKey)
	proxy.ServeHTTP(httptest.NewRecorder(), r)

	h := <-seen
	if got := h.Get("x-cstln-auth"); got != "" {
		t.Fatalf("node API key leaked to the backend: %q", got)
	}
	if got := h.Get("HTTP_X_CSTLN_AUTH"); got != "" {
		t.Fatalf("node API key leaked to the backend as a legacy header: %q", got)
	}
}
