package constellation

import (
	"github.com/azukaar/cosmos-server/src/utils"
	"strings"
	"sync"
)

var NebulaStarted = false
var NebulaHasStarted = false
var NATSStarted = false
var CachedDeviceNames = map[string]string{}
var CachedDevices = map[string]utils.ConstellationDevice{}
var needToSyncCA = false

// deviceCacheMux guards cachedCurrentDevice, CachedDevices and CachedDeviceNames,
// rebuilt from goroutines while readers run concurrently. Writers must REPLACE
// the maps wholesale so snapshots stay safe to range over after the lock is released.
var deviceCacheMux sync.RWMutex

// deviceCacheSnapshot returns the current cache maps under the read lock.
func deviceCacheSnapshot() (map[string]utils.ConstellationDevice, map[string]string) {
	deviceCacheMux.RLock()
	defer deviceCacheMux.RUnlock()
	return CachedDevices, CachedDeviceNames
}

func resyncConstellationNodes() {
	SendNewDBSyncMessage()
}

func getConstellationTunnelRoutes() []utils.ProxyRouteConfig {
	tunnels := GetLocalTunnelCache()
	routes := make([]utils.ProxyRouteConfig, len(tunnels))
	for i, t := range tunnels {
		routes[i] = t.Route
	}
	return routes
}

func GetDefaultHostnames() []string {
	hostnames, _ := utils.ListIps(true)
	httpHostname := utils.GetMainConfig().HTTPConfig.Hostname
	// Strip port if present
	if colonIndex := strings.LastIndex(httpHostname, ":"); colonIndex != -1 {
		httpHostname = httpHostname[:colonIndex]
	}
	if(utils.IsDomain(httpHostname) && !utils.IsLocalDomain(httpHostname)) {
		hostnames = append(hostnames, httpHostname)
	} else if httpHostname != "127.0.0.1" && httpHostname != "localhost" {
		hostnames = append(hostnames, httpHostname)
	}
	return hostnames
}

func InitHostname() {
	// if no hostname yet, set default one
	if utils.GetMainConfig().ConstellationConfig.ConstellationHostname == "" {
		utils.Log("Constellation: no hostname found, setting default one...")
		hostnames := GetDefaultHostnames()
		configFile := utils.ReadConfigFromFile()
		configFile.ConstellationConfig.ConstellationHostname = strings.Join(hostnames, ", ")
		utils.SetBaseMainConfig(configFile)
	} else {
		utils.Log("Constellation: hostname found: " + utils.GetMainConfig().ConstellationConfig.ConstellationHostname)
	}
}

func ConstellationConnected() bool {
	return utils.GetMainConfig().ConstellationConfig.Enabled && NebulaStarted
}

func IsConstellationIP(ip string) bool {
	if !ConstellationConnected() {
		return false
	}

	// Check if the IP exists in the cached device IPs
	_, names := deviceCacheSnapshot()
	for _, deviceIP := range names {
		if deviceIP == ip {
			return true
		}
	}

	return false
}

func GetConstellationFromIP(ip string) *utils.ConstellationDevice {
	if !ConstellationConnected() {
		return nil
	}

	// Check if the IP exists in the cached devices
	devices, _ := deviceCacheSnapshot()
	for _, device := range devices {
		if device.IP == ip {
			deviceCopy := device
			return &deviceCopy
		}
	}

	return nil
}

func Init() {
	utils.Log("Initializing Constellation module...")

	InitConfig()
	InitHostname()

	utils.IsConstellationIP = IsConstellationIP

	utils.ResyncConstellationNodes = resyncConstellationNodes
	utils.GetConstellationTunnelRoutes = getConstellationTunnelRoutes

	NebulaStarted = false
	NATSStarted = false

	var err error
	
	// if Constellation is enabled
	if utils.GetMainConfig().ConstellationConfig.Enabled {
		// populate CachedDeviceNames
		utils.Log("Constellation: populating device names cache...")
		c, closeDb, errCo := utils.GetEmbeddedCollection(utils.GetRootAppId(), "devices")
		defer closeDb()

		if errCo != nil {
			utils.Error("Database Connect", errCo)
		} else {
			cursor, err := c.Find(nil, map[string]interface{}{})

			if err != nil {
				utils.Error("DeviceList: Error fetching devices", err)
			} else {
				defer cursor.Close(nil)
				var devices []utils.ConstellationDevice

				if err = cursor.All(nil, &devices); err != nil {
					utils.Error("DeviceList: Error decoding devices", err)
				} else {
					// build into locals and swap once under the write lock,
					// so concurrent readers never see a half-built cache
					newNames := map[string]string{}
					newDevices := map[string]utils.ConstellationDevice{}
					for _, device := range devices {
						if device.Blocked {
							continue
						}
						newNames[device.DeviceName] = device.IP
						newDevices[device.DeviceName] = device
						utils.Debug("Constellation: device name cached: " + device.DeviceName + " -> " + device.IP)

						if device.PublicHostname != "" {
							publicHostnames := strings.Split(device.PublicHostname, ",")
							for _, publicHostname := range publicHostnames {
								newNames[strings.TrimSpace(publicHostname)] = device.IP
								newDevices[strings.TrimSpace(publicHostname)] = device
								utils.Debug("Constellation: public hostname cached: " + publicHostname + " -> " + device.IP)
							}
						}
					}

					// If current device is not in cache, populate from nebula.yml
					currentDeviceName, errName := GetCurrentDeviceName()
					if errName == nil && currentDeviceName != "" {
						if _, exists := newDevices[currentDeviceName]; !exists {
							utils.Log("Constellation: current device not in cache, populating from config...")
							currentDevice, errDevice := GetCurrentDevice()
							if errDevice == nil {
								newNames[currentDeviceName] = currentDevice.IP
								newDevices[currentDeviceName] = currentDevice
								utils.Debug("Constellation: current device cached: " + currentDeviceName + " -> " + currentDevice.IP)
							}
						}
					}

					deviceCacheMux.Lock()
					CachedDeviceNames = newNames
					CachedDevices = newDevices
					deviceCacheMux.Unlock()

					utils.Log("Constellation: device names cache populated")
				}
			}
		}

		// if !utils.GetMainConfig().ConstellationConfig.SlaveMode {
		// 	if !utils.FBL.LValid {
		// 		utils.MajorError("Constellation: No valid licence found to use Constellation. Disabling.", nil)
		// 		// disable constellation
		// 		configFile := utils.ReadConfigFromFile()
		// 		configFile.ConstellationConfig.Enabled = false
		// 		configFile.AdminConstellationOnly = false
		// 		utils.SetBaseMainConfig(configFile)
		// 		return
		// 	}

		// 	utils.Log("Initializing Constellation module...")
		// }

		// read isExitNode from config, if true, add masquerade to iptable
		populateIPTableMasquerade()

		// start nebula
		utils.Log("Constellation: starting nebula...")
		err = startNebula()
		if err != nil {
			utils.Error("Constellation: error while starting nebula", err)
			return
		}
	
		go InitDNS()
		go StartNATS()
		
		go InitPingLighthouses()

		utils.Log("Constellation module initialized")
	}
}