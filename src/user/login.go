package user

import (
	"encoding/json"
	"errors"
	"math/rand"
	"net/http"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/azukaar/cosmos-server/src/utils"
)

type LoginRequestJSON struct {
	Nickname string `validate:"required,min=3,max=32,alphanum"`
	Password string `validate:"required,min=8,max=128,containsany=~!@#$%^&*()_+=-{[}]:;"'<>.?/,containsany=ABCDEFGHIJKLMNOPQRSTUVWXYZ,containsany=abcdefghijklmnopqrstuvwxyz,containsany=0123456789"`
}

// UserLogin godoc
// @Summary User login
// @Description Authenticates a user with nickname and password, sets JWT cookie on success
// @Tags auth
// @Accept json
// @Produce json
// @Param request body LoginRequestJSON true "Login credentials"
// @Success 200 {object} utils.APIResponse
// @Failure 401 {object} utils.HTTPErrorResult
// @Failure 405 {object} utils.HTTPErrorResult
// @Failure 500 {object} utils.HTTPErrorResult
// @Router /api/login [post]
func UserLogin(w http.ResponseWriter, req *http.Request) {
	if req.Method == "POST" {
		time.Sleep(time.Duration(rand.Float64()*2) * time.Second)

		if utils.IsLoggedIn(req) {
			utils.Error("UserLogin: User already logged ing", nil)
			utils.HTTPError(w, "User is already logged in", http.StatusUnauthorized, "UL002")
			return
		}

		var request LoginRequestJSON
		err1 := json.NewDecoder(req.Body).Decode(&request)
		if err1 != nil {
			utils.Error("UserLogin: Invalid User Request", err1)
			utils.HTTPError(w, "User Login Error", http.StatusInternalServerError, "UL001")
			return
		}

		nickname := utils.Sanitize(request.Nickname)
		password := request.Password

		utils.Debug("UserLogin: Logging user " + nickname)

		user, err3 := utils.GetUser(nickname)

		if errors.Is(err3, utils.ErrNotFound) {
			bcrypt.CompareHashAndPassword([]byte("$2a$14$4nzsVwEnR3.jEbMTME7kqeCo4gMgR/Tuk7ivNExvXjr73nKvLgHka"), []byte("dummyPassword"))
			utils.Error("UserLogin: User not found", err3)
			utils.HTTPError(w, "User Logging Error", http.StatusInternalServerError, "UL001")
			return
		} else if err3 != nil {
			bcrypt.CompareHashAndPassword([]byte("$2a$14$4nzsVwEnR3.jEbMTME7kqeCo4gMgR/Tuk7ivNExvXjr73nKvLgHka"), []byte("dummyPassword"))
			utils.Error("UserLogin: Error while finding user", err3)
			utils.HTTPError(w, "User Logging Error", http.StatusInternalServerError, "UL001")
			return
		} else if user.Password == "" {
			utils.Error("UserLogin: User not registered", nil)
			utils.HTTPError(w, "User not registered", http.StatusInternalServerError, "UL002")
			return
		} else {
			err2 := bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(password))
			if err2 != nil {
				utils.Error("UserLogin: Encryption error", err2)
				utils.HTTPError(w, "User Logging Error", http.StatusInternalServerError, "UL001")
				return
			}

			if utils.IsEmailEnabled() && utils.IsNotifyLoginEmailEnabled() && user.Email != "" {
				clientIp := utils.GetClientIP(req)
				date := time.Now()
				if err := SendLoginNotificationEmail(user.Nickname, user.Email, clientIp, date); err != nil {
					utils.MajorError("UserLogin: Error while sending login notification email", err)
				}
			}

			SendUserToken(w, req, user, false, false)

			json.NewEncoder(w).Encode(map[string]interface{}{
				"status": "OK",
			})

			errE := utils.CommitMutation(utils.Mutation{
				Table:  "users",
				Op:     "update",
				Filter: map[string]interface{}{"Nickname": nickname},
				Doc: map[string]interface{}{
					"LastLogin": time.Now(),
				},
				BestEffort: true,
			})

			if errE != nil {
				utils.Error("UserLogin: Error while updating user last login", errE)
			}
		}
	} else {
		utils.Error("UserLogin: Method not allowed"+req.Method, nil)
		utils.HTTPError(w, "Method not allowed", http.StatusMethodNotAllowed, "HTTP001")
		return
	}
}
