package utils

import (
	"database/sql"
)

const userCols = "nickname, password, register_key, register_key_exp, role, password_cycle, email, registered_at, last_password_changed_at, created_at, last_login, mfa_key, was_2fa_verified"

type rowQuerier interface {
	Query(string, ...interface{}) (*sql.Rows, error)
}

func insertUserTx(tx *sql.Tx, u User) error {
	_, err := tx.Exec("INSERT INTO users ("+userCols+") VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)",
		u.Nickname, u.Password, u.RegisterKey, timeToDB(u.RegisterKeyExp), int(u.Role), u.PasswordCycle,
		u.Email, timeToDB(u.RegisteredAt), timeToDB(u.LastPasswordChangedAt), timeToDB(u.CreatedAt),
		timeToDB(u.LastLogin), u.MFAKey, boolToDB(u.Was2FAVerified))
	return err
}

func scanUsers(q rowQuerier, query string, args ...interface{}) ([]User, error) {
	rows, err := q.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// convention: on ANY error return nil, never a partial struct or slice —
	// call sites' fail-closed field checks depend on this
	users := []User{}
	for rows.Next() {
		var u User
		var role, was2fa int
		var regExp, regAt, lastPw, createdAt, lastLogin string
		if err := rows.Scan(&u.Nickname, &u.Password, &u.RegisterKey, &regExp, &role, &u.PasswordCycle,
			&u.Email, &regAt, &lastPw, &createdAt, &lastLogin, &u.MFAKey, &was2fa); err != nil {
			return nil, err
		}
		u.RegisterKeyExp = timeFromDB(regExp)
		u.Role = Role(role)
		u.RegisteredAt = timeFromDB(regAt)
		u.LastPasswordChangedAt = timeFromDB(lastPw)
		u.CreatedAt = timeFromDB(createdAt)
		u.LastLogin = timeFromDB(lastLogin)
		u.Was2FAVerified = was2fa != 0
		users = append(users, u)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return users, nil
}

// GetUser returns the user by nickname; on any error it returns a zero-value User
// (never partial data) so field checks at call sites stay fail-closed.
func GetUser(nickname string) (User, error) {
	db, err := getReadDB()
	if err != nil {
		return User{}, err
	}
	users, err := scanUsers(db, "SELECT "+userCols+" FROM users WHERE nickname = ?", nickname)
	if err != nil {
		return User{}, err
	}
	if len(users) == 0 {
		return User{}, ErrNotFound
	}
	return users[0], nil
}

// ListUsers returns users filtered by role ("admin", "user", or "" for all).
func ListUsers(role string) ([]User, error) {
	db, err := getReadDB()
	if err != nil {
		return nil, err
	}
	switch role {
	case "admin":
		return scanUsers(db, "SELECT "+userCols+" FROM users WHERE role = ?", int(ADMIN))
	case "user":
		return scanUsers(db, "SELECT "+userCols+" FROM users WHERE role = ?", int(USER))
	}
	return scanUsers(db, "SELECT "+userCols+" FROM users")
}

// ListAllUsers keeps the legacy signature for existing callers.
func ListAllUsers(role string) []User {
	users, err := ListUsers(role)
	if err != nil {
		Error("Database: Error while getting users", err)
		return []User{}
	}
	return users
}

func ListUsersPage(limit int) ([]User, error) {
	db, err := getReadDB()
	if err != nil {
		return nil, err
	}
	return scanUsers(db, "SELECT "+userCols+" FROM users ORDER BY nickname LIMIT ?", limit)
}

// CountUsers returns the number of users registered on this server.
func CountUsers() (int64, error) {
	db, err := getReadDB()
	if err != nil {
		return 0, err
	}
	var count int64
	err = db.QueryRow("SELECT COUNT(*) FROM users").Scan(&count)
	return count, err
}

func CreateUser(u User) error {
	return CommitMutation(Mutation{Table: "users", Op: "insert", Doc: u})
}

// UpdateUser sets the given bson-named fields on the user row.
func UpdateUser(nickname string, fields map[string]interface{}) error {
	return CommitMutation(Mutation{
		Table:  "users",
		Op:     "update",
		Filter: map[string]interface{}{"Nickname": nickname},
		Doc:    fields,
	})
}

func DeleteUser(nickname string) error {
	return CommitMutation(Mutation{
		Table:  "users",
		Op:     "delete",
		Filter: map[string]interface{}{"Nickname": nickname},
	})
}

func DeleteAllUsers() error {
	return CommitMutation(Mutation{Table: "users", Op: "deleteMany", Filter: map[string]interface{}{}})
}

// DeleteAllUsersLocal wipes users on this node only, never publishing. Reserved
// for fresh-install setup, where the intent is "this box starts empty" rather
// than "empty the cluster" — an empty filter compiles to an unqualified DELETE,
// so published it would take every user on every node with it.
func DeleteAllUsersLocal() error {
	return CommitMutationLocal(Mutation{Table: "users", Op: "deleteMany", Filter: map[string]interface{}{}})
}

// dump encoding uses bson field names and RFC3339Nano strings for canonical JSON
func dumpUser(u User) map[string]interface{} {
	return map[string]interface{}{
		"Nickname":              u.Nickname,
		"Password":              u.Password,
		"RegisterKey":           u.RegisterKey,
		"RegisterKeyExp":        timeToDB(u.RegisterKeyExp),
		"Role":                  int(u.Role),
		"PasswordCycle":         u.PasswordCycle,
		"Email":                 u.Email,
		"RegisteredAt":          timeToDB(u.RegisteredAt),
		"LastPasswordChangedAt": timeToDB(u.LastPasswordChangedAt),
		"CreatedAt":             timeToDB(u.CreatedAt),
		"LastLogin":             timeToDB(u.LastLogin),
		"MFAKey":                u.MFAKey,
		"Was2FAVerified":        u.Was2FAVerified,
	}
}

func parseDumpUser(m map[string]interface{}) User {
	return User{
		Nickname:              dumpStr(m, "Nickname"),
		Password:              dumpStr(m, "Password"),
		RegisterKey:           dumpStr(m, "RegisterKey"),
		RegisterKeyExp:        timeFromDB(dumpStr(m, "RegisterKeyExp")),
		Role:                  Role(dumpInt(m, "Role")),
		PasswordCycle:         dumpInt(m, "PasswordCycle"),
		Email:                 dumpStr(m, "Email"),
		RegisteredAt:          timeFromDB(dumpStr(m, "RegisteredAt")),
		LastPasswordChangedAt: timeFromDB(dumpStr(m, "LastPasswordChangedAt")),
		CreatedAt:             timeFromDB(dumpStr(m, "CreatedAt")),
		LastLogin:             timeFromDB(dumpStr(m, "LastLogin")),
		MFAKey:                dumpStr(m, "MFAKey"),
		Was2FAVerified:        dumpBool(m, "Was2FAVerified"),
	}
}

func dumpStr(m map[string]interface{}, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func dumpInt(m map[string]interface{}, key string) int {
	switch v := m[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return 0
}

func dumpBool(m map[string]interface{}, key string) bool {
	if v, ok := m[key].(bool); ok {
		return v
	}
	return false
}

