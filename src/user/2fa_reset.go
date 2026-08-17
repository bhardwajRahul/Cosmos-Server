package user

import (
	"encoding/json"
	"net/http"

	"github.com/azukaar/cosmos-server/src/utils"
)

type User2FAResetRequest struct {
	Nickname string `validate:"required"`
}

// Delete2FA godoc
// @Summary Reset 2FA for a user
// @Description Removes the 2FA key for a specified user (admin operation)
// @Tags auth
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body User2FAResetRequest true "Nickname of the user to reset 2FA for"
// @Success 200 {object} utils.APIResponse
// @Failure 401 {object} utils.HTTPErrorResult
// @Failure 403 {object} utils.HTTPErrorResult
// @Failure 500 {object} utils.HTTPErrorResult
// @Router /api/mfa [delete]
func Delete2FA(w http.ResponseWriter, req *http.Request) {
	if utils.CheckPermissions(w, req, utils.PERM_USERS) != nil {
		return
	}
	
	var request User2FAResetRequest
	errD := json.NewDecoder(req.Body).Decode(&request)
	if errD != nil {
		utils.Error("2FA Error: Invalid User Request", errD)
		utils.HTTPError(w, "2FA Error", http.StatusInternalServerError, "2FA001")
		return
	}

	nickname := request.Nickname

	userInBase, err := utils.GetUser(nickname)

	if err != nil {
		utils.Error("UserGet: Error while getting user", err)
		utils.HTTPError(w, "User Get Error", http.StatusInternalServerError, "2FA002")
		return
	}

	toSet := map[string]interface{}{
		"Was2FAVerified": false,
		"MFAKey": "",
		"PasswordCycle": userInBase.PasswordCycle + 1,
	}

	err = utils.UpdateUser(nickname, toSet)

	if err != nil {
		utils.Error("UserGet: Error while getting user", err)
		utils.HTTPError(w, "User Get Error", http.StatusInternalServerError, "2FA002")
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "OK",
	})
}