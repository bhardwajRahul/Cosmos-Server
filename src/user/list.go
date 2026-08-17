package user

import (
	"net/http"
	"encoding/json"
	"github.com/azukaar/cosmos-server/src/utils"
	"strconv"
	"math"
)

var maxLimit = 1000

// UserList godoc
// @Summary List all users
// @Description Returns a list of all users with optional limit
// @Tags users
// @Produce json
// @Security BearerAuth
// @Param limit query int false "Maximum number of users to return"
// @Success 200 {object} utils.APIResponse{data=[]utils.User}
// @Failure 401 {object} utils.HTTPErrorResult
// @Failure 403 {object} utils.HTTPErrorResult
// @Failure 405 {object} utils.HTTPErrorResult
// @Failure 500 {object} utils.HTTPErrorResult
// @Router /api/users [get]
func UserList(w http.ResponseWriter, req *http.Request) {
	if utils.CheckPermissions(w, req, utils.PERM_USERS_READ) != nil {
		return
	} 

	limit, _ := strconv.Atoi(req.URL.Query().Get("limit"))
	// from, _ := req.URL.Query().Get("from")

	if limit == 0 {
		limit = maxLimit
	}
	
	if(req.Method == "GET") {
		utils.Debug("UserList: List user ")

		l := int(math.Max((float64)(maxLimit), (float64)(limit)))

		// TODO: Implement pagination

		users, errDB := utils.ListUsersPage(l)

		if errDB != nil {
			utils.Error("UserList: Error while getting user", errDB)
			utils.HTTPError(w, "User Get Error", http.StatusInternalServerError, "UL001")
			return
		}

		userList := []utils.User{}
		for _, user := range users {
			user.Link = "/api/user/" + user.Nickname
			userList = append(userList, user)
		}


		json.NewEncoder(w).Encode(map[string]interface{}{
			"status": "OK",
			"data": userList,
		})
	} else {
		utils.Error("UserList: Method not allowed" + req.Method, nil)
		utils.HTTPError(w, "Method not allowed", http.StatusMethodNotAllowed, "HTTP001")
		return
	}
}