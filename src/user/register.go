package user

import (
	"net/http"
	"math/rand"
	"encoding/json"
	"errors"
	"time"
	"golang.org/x/crypto/bcrypt"

	"github.com/azukaar/cosmos-server/src/utils"
)

type RegisterRequestJSON struct {
	Nickname string `validate:"required,min=3,max=32,alphanum"`
	Password string `validate:"required,min=9,max=128,containsany=~!@#$%^&*()_+=-{[}]:;"'<>.?/,containsany=ABCDEFGHIJKLMNOPQRSTUVWXYZ,containsany=abcdefghijklmnopqrstuvwxyz,containsany=0123456789"`
	RegisterKey string `validate:"required,min=1,max=512,alphanum"`
}

// UserRegister godoc
// @Summary Register a new user
// @Description Completes user registration using a registration key (invite link)
// @Tags auth
// @Accept json
// @Produce json
// @Param request body RegisterRequestJSON true "Registration details including register key"
// @Success 200 {object} utils.APIResponse
// @Failure 401 {object} utils.HTTPErrorResult
// @Failure 405 {object} utils.HTTPErrorResult
// @Failure 500 {object} utils.HTTPErrorResult
// @Router /api/register [post]
func UserRegister(w http.ResponseWriter, req *http.Request) {
	if(req.Method == "POST") {
		time.Sleep(time.Duration(rand.Float64()*2)*time.Second)
		
		var request RegisterRequestJSON
		err1 := json.NewDecoder(req.Body).Decode(&request)
		if err1 != nil {
			utils.Error("UserRegister: Invalid User Request", err1)
			utils.HTTPError(w, "User Register Error", http.StatusInternalServerError, "UR001")
			return
		}

		errV := utils.Validate.Struct(request)
		if errV != nil {
			utils.Error("UserRegister: Invalid User Request", errV)
			utils.HTTPError(w, "User Register Error: " + errV.Error(), http.StatusInternalServerError, "UR002")
			return
		}

		nickname := utils.Sanitize(request.Nickname)
		password := request.Password
		registerKey := request.RegisterKey

		utils.Debug("UserRegister: Registering user " + nickname)
				
		hashedPassword, err2 := bcrypt.GenerateFromPassword([]byte(password), 14)

		if err2 != nil {
			utils.Error("UserRegister: Encryption error", err2)
			utils.HTTPError(w, "User Register Error", http.StatusUnauthorized, "UR001")
			return
		}

		user, err3 := utils.GetUser(nickname)
		// key mismatch behaves like not-found, matching the old compound filter
		if err3 == nil && user.RegisterKey != registerKey {
			user = utils.User{}
			err3 = utils.ErrNotFound
		}

		if errors.Is(err3, utils.ErrNotFound) {
			utils.Error("UserRegister: User not found", err3)
			utils.HTTPError(w, "User Register Error", http.StatusInternalServerError, "UR001")
			return
		} else if err3 != nil {
			utils.Error("UserRegister: Error while finding user", err3)
			utils.HTTPError(w, "User Register Error", http.StatusInternalServerError, "UR001")
			return
		} else if user.RegisterKeyExp.Before(time.Now()) {
			utils.Error("UserRegister: Link expired", nil)
			utils.HTTPError(w, "User Register Error", http.StatusInternalServerError, "UR001")
			return
		} else {
			RegisteredAt := user.RegisteredAt
			if RegisteredAt.IsZero() {
				RegisteredAt = time.Now()
			}
			err4 := utils.CommitMutation(utils.Mutation{
				Table: "users",
				Op:    "update",
				Filter: map[string]interface{}{
					"Nickname": nickname,
					"RegisterKey": registerKey,
				},
				Doc: map[string]interface{}{
					"Password": hashedPassword,
					"RegisterKey": "",
					"RegisterKeyExp": time.Time{},
					"RegisteredAt": RegisteredAt,
					"LastPasswordChangedAt": time.Now(),
					"PasswordCycle": user.PasswordCycle + 1,
				},
			})

			if err4 != nil {
				utils.Error("UserRegister: Error while updating user", err4)
				utils.HTTPError(w, "User Register Error", http.StatusInternalServerError, "UR001")
				return
			}
		}
		
		utils.TriggerEvent(
			"cosmos.user.register",
			"User registered",
			"success",
			"",
			map[string]interface{}{
				"nickname": nickname,
		})

		json.NewEncoder(w).Encode(map[string]interface{}{
			"status": "OK",
		})

	} else {
		utils.Error("UserRegister: Method not allowed" + req.Method, nil)
		utils.HTTPError(w, "Method not allowed", http.StatusMethodNotAllowed, "HTTP001")
		return
	}
}