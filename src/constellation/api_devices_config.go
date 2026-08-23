package constellation

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/azukaar/cosmos-server/src/utils"
)

type DeviceResyncRequest struct {
	Nickname string `json:"nickname" validate:"required,max=32"`
	DeviceName string `json:"deviceName" validate:"required,min=3,max=32"`
}

// GetDeviceConfigManualSync rebuilds a device's config (no pki/api key) for a resync QR
func GetDeviceConfigManualSync(w http.ResponseWriter, req *http.Request) {
	if req.Method != "POST" {
		utils.Error("DeviceConfigManualSync: Method not allowed" + req.Method, nil)
		utils.HTTPError(w, "Method not allowed", http.StatusMethodNotAllowed, "HTTP001")
		return
	}

	var request DeviceResyncRequest
	if err := json.NewDecoder(req.Body).Decode(&request); err != nil {
		utils.Error("DeviceConfigManualSync: Invalid User Request", err)
		utils.HTTPError(w, "Device Resync Error", http.StatusBadRequest, "DB001")
		return
	}
	if err := utils.Validate.Struct(request); err != nil {
		utils.Error("DeviceConfigManualSync: Invalid User Request", err)
		utils.HTTPError(w, "Device Resync Error: " + err.Error(), http.StatusBadRequest, "DB002")
		return
	}

	nickname := utils.Sanitize(request.Nickname)
	deviceName := utils.Sanitize(request.DeviceName)

	if utils.CheckPermissionsOrSelf(w, req, nickname, utils.PERM_RESOURCES) != nil {
		return
	}

	devices, err := utils.FindDevices(map[string]interface{}{
		"Nickname": nickname,
		"DeviceName": deviceName,
	})
	if err != nil {
		utils.Error("DeviceConfigManualSync: Error fetching devices", err)
		utils.HTTPError(w, "Error fetching devices", http.StatusInternalServerError, "DL003")
		return
	}
	if len(devices) == 0 {
		utils.Error("DeviceConfigManualSync: device not found", nil)
		utils.HTTPError(w, "Device not found", http.StatusNotFound, "DCS001")
		return
	}

	utils.Log("DeviceConfigManualSync: Resync Device " + deviceName)

	d := devices[0]
	d.PublicKey = ""
	d.APIKey = ""
	configYml, err := getYAMLClientConfig(d.DeviceName, utils.CONFIGFOLDER + "nebula.yml", "", "", "", "", d, true, true)
	if err != nil {
		utils.Error("DeviceConfigManualSync: Error marshalling nebula.yml", err)
		utils.HTTPError(w, "Error marshalling nebula.yml", http.StatusInternalServerError, "DCS003")
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "OK",
		"data": string(configYml),
	})
}

func compareConfigs(configMap, configMapNew map[string]interface{}) bool {
	configMapStr := fmt.Sprintf("%+v", configMap)
	configMapNewStr := fmt.Sprintf("%+v", configMapNew)

	if string(configMapStr) == string(configMapNewStr) {
		return true
	} else {
		return false
	}
}


func setDefaultConstConfig(configMap map[string]interface{}) map[string]interface{} {
	utils.Debug("setDefaultConstConfig: Setting default values for client nebula.yml")

	if _, exists := configMap["logging"]; !exists {
			configMap["logging"] = map[string]interface{}{
					"format": "text",
					"level":  "info",
			}
	}

	if _, exists := configMap["listen"]; !exists {
			configMap["listen"] = map[string]interface{}{
					"host": "0.0.0.0",
					"port": 4242,
			}
	}

	if _, exists := configMap["punchy"]; !exists {
			configMap["punchy"] = map[string]interface{}{
					"punch":    true,
					"respond":  true,
			}
	}

	if _, exists := configMap["tun"]; !exists {
			configMap["tun"] = map[string]interface{}{
					"dev":                  "nebula1",
					"disabled":             false,
					"drop_local_broadcast": false,
					"drop_multicast":       false,
					"mtu":                  1300,
					"routes":               []interface{}{},
					"tx_queue":             500,
					"unsafe_routes":        []interface{}{},
			}
	}

	if _, exists := configMap["firewall"]; !exists {
			configMap["firewall"] = map[string]interface{}{
					"conntrack": map[string]interface{}{
							"default_timeout": "10m",
							"tcp_timeout":     "12m",
							"udp_timeout":     "3m",
					},
					"inbound": []interface{}{
							map[string]interface{}{
									"host":  "any",
									"port":  "any",
									"proto": "any",
							},
					},
					"inbound_action": "drop",
					"outbound": []interface{}{
							map[string]interface{}{
									"host":  "any",
									"port":  "any",
									"proto": "any",
							},
					},
					"outbound_action": "drop",
			}
	}

	return configMap
}
