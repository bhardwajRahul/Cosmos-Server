package configapi

import (
	"net/http"
	"encoding/json"
	"github.com/azukaar/cosmos-server/src/constellation"
	"github.com/azukaar/cosmos-server/src/utils"
)

// publishReplicatedDomains publishes the three op-log domains this whole-config
// PUT can touch. Full-state ops are idempotent, so publishing all three
// unconditionally is cheaper than diffing and cannot miss a change. APITokens and
// the auth keypair are restored from disk above, so they are not editable here.
func publishReplicatedDomains(request utils.Config) error {
	c := request.ConstellationConfig
	err := constellation.PublishDomainOp(constellation.DomainDNS, constellation.DNSPayload{
		DNSPort:                 c.DNSPort,
		DNSFallback:             c.DNSFallback,
		DNSBlockBlacklist:       c.DNSBlockBlacklist,
		DNSAdditionalBlocklists: c.DNSAdditionalBlocklists,
		CustomDNSEntries:        c.CustomDNSEntries,
	})
	if err != nil {
		return err
	}
	if err := constellation.PublishDomainOp(constellation.DomainRoles, request.Roles); err != nil {
		return err
	}
	return constellation.PublishDomainOp(constellation.DomainOpenIDClients, request.OpenIDClients)
}

// restoreReplicatedDomains takes the replicated fields from the freshly applied
// config rather than from the request, so the local write can never roll back
// what the apply loop just installed. Same treatment the auth keypair and API
// tokens already get. Every OTHER ConstellationConfig field (Enabled, IPRange,
// ThisDeviceName, DNSDisabled...) stays node-local from the request by design.
func restoreReplicatedDomains(request *utils.Config, config utils.Config) {
	request.ConstellationConfig.DNSPort = config.ConstellationConfig.DNSPort
	request.ConstellationConfig.DNSFallback = config.ConstellationConfig.DNSFallback
	request.ConstellationConfig.DNSBlockBlacklist = config.ConstellationConfig.DNSBlockBlacklist
	request.ConstellationConfig.DNSAdditionalBlocklists = config.ConstellationConfig.DNSAdditionalBlocklists
	request.ConstellationConfig.CustomDNSEntries = config.ConstellationConfig.CustomDNSEntries
	request.Roles = config.Roles
	request.OpenIDClients = config.OpenIDClients
}

// ConfigApiSet godoc
// @Summary Update server configuration
// @Description Replaces the entire server configuration (masked credential fields are preserved from existing config)
// @Tags config
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body utils.Config true "Full configuration object"
// @Success 200 {object} utils.APIResponse
// @Failure 401 {object} utils.HTTPErrorResult
// @Failure 403 {object} utils.HTTPErrorResult
// @Failure 405 {object} utils.HTTPErrorResult
// @Failure 500 {object} utils.HTTPErrorResult
// @Router /api/config [put]
func ConfigApiSet(w http.ResponseWriter, req *http.Request) {
	if utils.CheckPermissions(w, req, utils.PERM_CONFIGURATION) != nil {
		return
	} 

	if(req.Method == "PUT") {
		var request utils.Config
		err1 := json.NewDecoder(req.Body).Decode(&request)
		if err1 != nil {
			utils.Error("SettingsUpdate: Invalid User Request", err1)
			utils.HTTPError(w, "User Creation Error", 
				http.StatusInternalServerError, "UC001")
			return 
		}

		errV := utils.Validate.Struct(request)
		if errV != nil {
			utils.Error("SettingsUpdate: Invalid User Request", errV)
			utils.HTTPError(w, "User Creation Error: " + errV.Error(),
				http.StatusInternalServerError, "UC003")
			return 
		}

		// restore fields that are never sent to the client or are masked with ***
		config := utils.ReadConfigFromFile()
		request.HTTPConfig.AuthPrivateKey = config.HTTPConfig.AuthPrivateKey
		request.HTTPConfig.AuthPublicKey = config.HTTPConfig.AuthPublicKey
		request.HTTPConfig.TLSCert = config.HTTPConfig.TLSCert
		request.HTTPConfig.TLSKey = config.HTTPConfig.TLSKey
		request.NewInstall = config.NewInstall

		// restore API token as we cannot edit it here
		request.APITokens = config.APITokens

		// restore DNS if user cannot read credentials, as they are sent as empty and we don't want to override them
		canReadCredentials := utils.HasPermission(req, utils.PERM_CREDENTIALS_READ)
		if !canReadCredentials {
			request.HTTPConfig.DNSChallengeConfig = config.HTTPConfig.DNSChallengeConfig
		}

		// restore credential fields if they were masked (sent as "***")
		if request.MongoDB == "***" {
			request.MongoDB = config.MongoDB
		}
		if request.EmailConfig.Password == "***" {
			request.EmailConfig.Password = config.EmailConfig.Password
		}
		if request.EmailConfig.Username == "***" {
			request.EmailConfig.Username = config.EmailConfig.Username
		}
		if request.EmailConfig.Host == "***" {
			request.EmailConfig.Host = config.EmailConfig.Host
		}
		if request.Database.Password == "***" {
			request.Database.Password = config.Database.Password
		}
		if request.Database.Username == "***" {
			request.Database.Username = config.Database.Username
		}
		if request.Licence == "***" {
			request.Licence = config.Licence
		}
		if request.ServerToken == "***" {
			request.ServerToken = config.ServerToken
		}

		// restore backup passwords. Users without PERM_CREDENTIALS_READ can never
		// set a password — always restore from existing config. Otherwise only
		// restore when the value is missing or masked, so the settings UI
		// round-trip doesn't wipe it.
		for name, b := range request.Backup.Backups {
			shouldRestore := !canReadCredentials || b.Password == "" || b.Password == "***"
			if shouldRestore {
				if existing, ok := config.Backup.Backups[name]; ok && existing.Password != "" {
					b.Password = existing.Password
					request.Backup.Backups[name] = b
				}
			}
		}

		// Replicated domains go through the op-log, never straight to disk. Published
		// BEFORE the local write so a read-only node fails here with nothing written,
		// rather than leaving a node-local edit the log will never hear about and the
		// next snapshot will silently discard. ConfigLock must NOT be held across
		// these — the apply loop takes it to install the op we are waiting on.
		if err := publishReplicatedDomains(request); err != nil {
			utils.HTTPStoreError(w, err, "UC004")
			return
		}

		// The local write is the node-local half. Under the lock so it cannot
		// interleave with an apply-loop domain install, and re-reading here picks up
		// the state that loop just applied.
		utils.ConfigLock.Lock()
		config = utils.ReadConfigFromFile()
		restoreReplicatedDomains(&request, config)
		utils.SetBaseMainConfig(request)
		utils.ConfigLock.Unlock()

		utils.TriggerEvent(
			"cosmos.settings",
			"Settings updated",
			"success",
			"",
			map[string]interface{}{
		})

		go utils.SoftRestartServer()

		json.NewEncoder(w).Encode(map[string]interface{}{
			"status": "OK",
		})
	} else {
		utils.Error("SettingsUpdate: Method not allowed" + req.Method, nil)
		utils.HTTPError(w, "Method not allowed", http.StatusMethodNotAllowed, "HTTP001")
		return
	}
}
