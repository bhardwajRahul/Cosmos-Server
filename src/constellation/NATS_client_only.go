package constellation

import (
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/azukaar/cosmos-server/src/utils"
)

// Client-only nodes (CosmosNode == 0) don't run NATS; they fetch the public
// device list from a manager and persist it locally.

var clientSyncOK atomic.Bool
var clientSyncGen atomic.Int64

func IsClientNode() bool {
	d, err := GetCurrentDevice()
	return err == nil && d.CosmosNode == 0
}

func IsClientSynced() bool {
	return clientSyncOK.Load()
}

// seed managers first, then lighthouse hosts, sorted
func clientSyncTargets() []string {
	seen := map[string]bool{}
	targets := []string{}
	add := func(ip string) {
		ip = cleanIp(ip)
		if ip != "" && !seen[ip] {
			seen[ip] = true
			targets = append(targets, ip)
		}
	}
	seeds := getSeedManagerIPs()
	sort.Strings(seeds)
	for _, ip := range seeds {
		add(ip)
	}
	lhs, _ := GetAllLighthouseIPFromTempConfig()
	sort.Strings(lhs)
	for _, ip := range lhs {
		add(ip)
	}
	return targets
}

func fetchPublicDevicesFrom(ip string) ([]PublicDeviceInfo, error) {
	user, pwd, err := GetNATSCredentials()
	if err != nil {
		return nil, err
	}
	conn, err := connectNATSClient("nats://"+ip+":4222", user, pwd)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	msg, err := conn.Request(publicDevicesSubject, []byte{}, 8*time.Second)
	if err != nil {
		return nil, err
	}

	var reply struct {
		Status string             `json:"status"`
		Data   []PublicDeviceInfo `json:"data"`
	}
	if err := json.Unmarshal(msg.Data, &reply); err != nil {
		return nil, err
	}
	if reply.Status != "OK" || reply.Data == nil {
		return nil, errors.New("malformed public-devices reply from " + ip)
	}
	return reply.Data, nil
}

// persistPublicDevices replaces the local device table (minus self) so nebula
// restarts pick up the right lighthouses/relays/firewall.
func persistPublicDevices(list []PublicDeviceInfo) error {
	self, _ := GetCurrentDeviceName()

	if err := utils.CommitMutationLocal(utils.Mutation{Table: "devices", Op: "deleteMany", Filter: map[string]interface{}{}}); err != nil {
		return err
	}
	for _, p := range list {
		if p.DeviceName == "" || p.DeviceName == self {
			continue
		}
		d := utils.ConstellationDevice{
			Nickname:       p.User,
			DeviceName:     p.DeviceName,
			IP:             p.IP,
			IsLighthouse:   p.IsLighthouse,
			CosmosNode:     p.CosmosNode,
			IsRelay:        p.IsRelay,
			IsExitNode:     p.IsExitNode,
			PublicHostname: p.PublicHostname,
			Port:           p.Port,
		}
		if err := utils.CommitMutationLocal(utils.Mutation{Table: "devices", Op: "insert", Doc: d}); err != nil {
			utils.Warn("[CLIENT] could not persist device " + p.DeviceName + ": " + err.Error())
		}
	}
	return nil
}

func clientSyncOnce() error {
	targets := clientSyncTargets()
	if len(targets) == 0 {
		return errors.New("no manager or lighthouse to fetch devices from")
	}
	var lastErr error
	for _, ip := range targets {
		list, err := fetchPublicDevicesFrom(ip)
		if err != nil {
			lastErr = err
			utils.Debug("[CLIENT] public-devices fetch from " + ip + " failed: " + err.Error())
			continue
		}
		if err := persistPublicDevices(list); err != nil {
			return err
		}
		utils.Log("[CLIENT] synced " + strconv.Itoa(len(list)) + " devices from " + ip)
		refreshDeviceCache()
		return nil
	}
	return lastErr
}

// StartClientDeviceSync does one fetch on connect, retried up to 5 times
// every 15s. A restart bumps the generation so a stale attempt exits.
func StartClientDeviceSync() {
	gen := clientSyncGen.Add(1)
	alive := func() bool { return NebulaStarted.Load() && clientSyncGen.Load() == gen }

	clientSyncOK.Store(false)
	for attempt := 1; attempt <= 5 && alive(); attempt++ {
		err := clientSyncOnce()
		if err == nil {
			clientSyncOK.Store(true)
			return
		}
		utils.Warn("[CLIENT] device fetch failed (attempt " + strconv.Itoa(attempt) + "/5): " + err.Error())
		for end := time.Now().Add(15 * time.Second); alive() && time.Now().Before(end); {
			time.Sleep(time.Second)
		}
	}
}
