package constellation

import (
	"net/http"
	"encoding/json"
	"errors"
	"regexp"
	"sync"

	"github.com/azukaar/cosmos-server/src/utils"
)

// deviceCreateMutex serializes DeviceCreate to close the name/IP check-then-insert race
var deviceCreateMutex sync.Mutex

type DeviceCreateRequestJSON struct {
	DeviceName string `json:"deviceName" validate:"required,min=3,max=32"`
	IP string `json:"ip" validate:"required,ipv4"`

	// for devices only
	Nickname string `json:"nickname,omitempty" validate:"omitempty,max=32"`
	Invisible bool `json:"invisible,omitempty"`

	// for lighthouse only
	IsLighthouse bool `json:"isLighthouse,omitempty"`
	CosmosNode int `json:"cosmosNode,omitempty"`
	IsRelay bool `json:"isRelay,omitempty"`
	IsLoadBalancer bool `json:"isLoadBalancer,omitempty"`
	IsExitNode bool `json:"isExitNode,omitempty"`
	PublicHostname string `json:"PublicHostname,omitempty"`
	Port string `json:"port,omitempty"`

	// internal
	APIKey string `json:"-"`
}

// DeviceCreate_API godoc
// @Summary Create a new Constellation device and generate its certificates
// @Tags constellation
// @Accept json
// @Produce json
// @Param body body DeviceCreateRequestJSON true "Device creation payload"
// @Security BearerAuth
// @Success 200 {object} utils.APIResponse
// @Failure 403 {object} utils.HTTPErrorResult
// @Failure 500 {object} utils.HTTPErrorResult
// @Router /api/constellation/devices [post]
func DeviceCreate_API(w http.ResponseWriter, req *http.Request) {
	if(req.Method == "POST") {
		var request DeviceCreateRequestJSON
		err1 := json.NewDecoder(req.Body).Decode(&request)
		if err1 != nil {
			utils.Error("ConstellationDeviceCreation: Invalid User Request", err1)
			utils.HTTPError(w, "Device Creation Error",
				http.StatusInternalServerError, "DC001")
			return 
		}

		if !utils.FBL.LValid {
			utils.Error("ConstellationDeviceCreation: No valid licence found to use Constellation.", nil)
			utils.HTTPError(w, "Device Creation Error: No valid licence found to use Constellation.",
				http.StatusInternalServerError, "DC001")
			return
		}

		if utils.FBL.AgentMode {
			utils.Error("ConstellationDeviceCreation: Agents cannot create devices. Use a manager server", nil)
			utils.HTTPError(w, "Device Creation Error: Agents cannot create devices. Use a manager server",
				http.StatusInternalServerError, "DC001")
			return
		}

		nickname := utils.Sanitize(request.Nickname)

		if utils.CheckPermissionsOrSelf(w, req, nickname, utils.PERM_RESOURCES) != nil {
			return
		}

		// Non-admin users can only create client devices
		if !utils.HasPermission(req, utils.PERM_RESOURCES) {
			if request.IsLighthouse {
				utils.Error("DeviceCreation: Non-admin users can only create client devices", nil)
				utils.HTTPError(w, "Device Creation Error: Only administrators can create lighthouse devices",
					http.StatusForbidden, "DC006")
				return
			}

			request.CosmosNode = 0
		}

		errV := utils.Validate.Struct(request)
		if errV != nil {
			utils.Error("DeviceCreation: Invalid User Request", errV)
			utils.HTTPError(w, "Device Creation Error: " + errV.Error(),
				http.StatusInternalServerError, "DC002")
			return 
		}

		cert, key, _, request, err := DeviceCreate(request)
		if err != nil {
			utils.Error("DeviceCreation: Error creating device", err)
			utils.HTTPError(w, "Device Creation Error: " + err.Error(),
				http.StatusInternalServerError, "DC003")
			return
		}

		APIKey := request.APIKey
		deviceName := request.DeviceName
		
		capki, err := getCApki()
		if err != nil {
			utils.Error("DeviceCreation: Error while reading CA", err)
			utils.HTTPError(w, "Device Creation Error: " + err.Error(),
				http.StatusInternalServerError, "DC003")
			return
		}

		if err == nil {
			// read configYml from config/nebula.yml
			configYml, err := getYAMLClientConfig(deviceName, utils.CONFIGFOLDER + "nebula.yml", capki, cert, key, APIKey, utils.ConstellationDevice{
				Nickname: nickname,
				DeviceName: deviceName,
				IP: request.IP,
				IsLighthouse: request.IsLighthouse,
				CosmosNode: request.CosmosNode,
				IsRelay: request.IsRelay,
				IsExitNode: request.IsExitNode,
				IsLoadBalancer: request.IsLoadBalancer,
				PublicHostname: request.PublicHostname,
				Port: request.Port,
				APIKey: APIKey,
			}, true, true)

			if err != nil {
				utils.Error("DeviceCreation: Error while reading config", err)
				utils.HTTPError(w, "Device Creation Error: " + err.Error(), http.StatusInternalServerError, "DC004") 
				return
			}

			lightHousesList := []utils.ConstellationDevice{}
			if request.IsLighthouse {
				lightHousesList, err = GetAllLightHouses()
			}

			if err != nil {
				utils.Error("DeviceCreation: Error while reading config", err)
				utils.HTTPError(w, "Device Creation Error: " + err.Error(),
					http.StatusInternalServerError, "DC005")
				return
			}

			utils.TriggerEvent(
				"cosmos.constellation.device.create",
				"Device created",
				"success",
				"",
				map[string]interface{}{
					"deviceName": deviceName,
					"nickname": nickname,
					"ip": request.IP,
			})

			json.NewEncoder(w).Encode(map[string]interface{}{
				"status": "OK",
				"data": map[string]interface{}{
					"Nickname": nickname,
					"DeviceName": deviceName,
					"Certificate": cert,
					"PrivateKey": key,
					"IP": request.IP,
					"Config": configYml,
					"CA": capki,
					"IsLighthouse": request.IsLighthouse,
					"CosmosNode": request.CosmosNode,
					"IsLoadBalancer": request.IsLoadBalancer,
					"IsRelay": request.IsRelay,
					"IsExitNode": request.IsExitNode,
					"PublicHostname": request.PublicHostname,
					"Port": request.Port,
					"LighthousesList": lightHousesList,
					"Invisible": request.Invisible,
				},
			})
		} else {
			utils.Error("DeviceCreation: Error creating device", err)
			utils.HTTPStoreError(w, err, "DC004")
			return
		}
	} else {
		utils.Error("DeviceCreation: Method not allowed" + req.Method, nil)
		utils.HTTPError(w, "Method not allowed", http.StatusMethodNotAllowed, "HTTP001")
		return
	}
}

var deviceNameRe = regexp.MustCompile(`^[a-z0-9_-]{3,32}$`)

func DeviceCreate(request DeviceCreateRequestJSON) (string, string, string, DeviceCreateRequestJSON, error) {
	deviceCreateMutex.Lock()
	defer deviceCreateMutex.Unlock()

	nickname := utils.Sanitize(request.Nickname)
	deviceName := utils.Sanitize(request.DeviceName)
	// name is used raw as NATS user, subject token and KV key: no spaces, dots or wildcards
	if !deviceNameRe.MatchString(deviceName) {
		return "", "", "", request, errors.New("Device name must be 3-32 characters of a-z, 0-9, '-' or '_'")
	}
	APIKey := utils.GenerateRandomString(32)

	if request.Port == "" {
		request.Port = "4242"
	}

	utils.Log("ConstellationDeviceCreation: Creating Device " + deviceName)

	utils.Debug("ConstellationDeviceCreation: Creating Device " + deviceName)

	// read-then-write pre-checks kept for their error messages; the partial
	// unique indexes are the authority
	_, err2 := utils.GetDeviceByName(deviceName, true)

	if err2 == nil {
		return "", "", "", DeviceCreateRequestJSON{}, errors.New("DeviceCreation: Device with this name already exists")
	} else if !errors.Is(err2, utils.ErrNotFound) {
		return "", "", "", DeviceCreateRequestJSON{}, err2
	}

	// Check if IP is already in use
	_, errIP := utils.GetDeviceByIP(request.IP)

	if errIP == nil {
		return "", "", "", DeviceCreateRequestJSON{}, errors.New("DeviceCreation: IP address is already in use")
	} else if !errors.Is(errIP, utils.ErrNotFound) {
		return "", "", "", DeviceCreateRequestJSON{}, errIP
	}

	// Device name and IP are both available, proceed with creation
	{
		cert, key, fingerprint, err := generateNebulaCert(deviceName, deviceName, request.IP, false)

		if err != nil {
			return "", "", "", DeviceCreateRequestJSON{}, err
		}

		// Check cosmos node and devices limit (skipped on Pro — unlimited).
		if !utils.IsPro() && request.CosmosNode > 0 {
			countManagers, errCount := utils.CountDevices(map[string]interface{}{
				"CosmosNode": 2,
				"Blocked": false,
			})

			if errCount != nil {
				return "", "", "", DeviceCreateRequestJSON{}, errCount
			}

			countAgent, errCount := utils.CountDevices(map[string]interface{}{
				"CosmosNode": 1,
				"Blocked": false,
			})

			if errCount != nil {
				return "", "", "", DeviceCreateRequestJSON{}, errCount
			}

			totalCount := countManagers + countAgent

			if totalCount >= int64(utils.GetNumberCosmosNode()) {
				// we are creating the extra agent allowed in the licence
				if request.CosmosNode == 1 && totalCount == int64(utils.GetNumberCosmosNode()) {

				// We are creating a manager but one slot was already taken by an agent using the extra slot allowed in the licence
				} else if request.CosmosNode == 2 && totalCount == int64(utils.GetNumberCosmosNode()) &&
					countAgent >= 1 {

				} else {
					return "", "", "", DeviceCreateRequestJSON{}, errors.New("DeviceCreation: Cosmos Node limit reached")
				}
			}
		}

		if !utils.IsPro() {
			totalClientLimit := 10 * int64(utils.GetNumberUsers())

			countDevices, errCountDevices := utils.CountDevices(map[string]interface{}{
				"Blocked": false,
			})

			if errCountDevices != nil {
				return "", "", "", DeviceCreateRequestJSON{}, errCountDevices
			}

			if countDevices >= totalClientLimit {
				return "", "", "", DeviceCreateRequestJSON{}, errors.New("DeviceCreation: Device limit reached")
			}
		}

		if request.IsLighthouse && request.Nickname != "" {
			return "", "", "", DeviceCreateRequestJSON{}, errors.New("DeviceCreation: Lighthouse cannot belong to a user")
		}

		if err != nil {
			return "", "", "", DeviceCreateRequestJSON{}, err
		}

		if (!request.IsLighthouse ) {
			request.IsRelay = false
			request.IsExitNode = false
			request.Invisible = false
			request.IsLoadBalancer = false
		}

		err3 := utils.CreateDevice(utils.ConstellationDevice{
			Nickname: nickname,
			DeviceName: deviceName,
			IP: request.IP,
			IsLighthouse: request.IsLighthouse,
			CosmosNode: request.CosmosNode,
			IsRelay: request.IsRelay,
			IsExitNode: request.IsExitNode,
			PublicHostname: request.PublicHostname,
			IsLoadBalancer: request.IsLoadBalancer,
			Port: request.Port,
			Fingerprint: fingerprint,
			APIKey: APIKey,
			Blocked: false,
			Invisible: request.Invisible,
		})

		if err3 != nil {
			return "", "", "", DeviceCreateRequestJSON{}, err3
		} 

		request.Nickname = nickname
		request.DeviceName = deviceName
		request.APIKey = APIKey

		return cert, key, fingerprint, request, nil
	}
}