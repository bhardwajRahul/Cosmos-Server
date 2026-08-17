package utils

import (
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// Mutation is the single write descriptor; Filter/Doc keys use bson field names.
type Mutation struct {
	Table      string
	Op         string // insert|insertMany|update|updateMany|delete|deleteMany
	Filter     map[string]interface{}
	Doc        interface{}
	BestEffort bool // LastLogin only
}

// CommitMutation routes one mutation through the op-log when it is wired, and
// writes it directly otherwise. The hook itself decides between publishing and
// falling back to a direct tx (standalone install, pre-NATS boot).
func CommitMutation(m Mutation) error {
	if hook := getPublishOpHook(); hook != nil {
		return hook(m)
	}
	return CommitMutations([]Mutation{m})
}

// CommitMutations applies several mutations in one direct tx; never published
// (migrations and the op-log's own direct fallback are its only callers).
func CommitMutations(ms []Mutation) error {
	db, err := getWriteDB()
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	for _, m := range ms {
		if err := applyMutation(tx, m); err != nil {
			tx.Rollback()
			return mapSQLError(err)
		}
	}
	return tx.Commit()
}

// applyMutation is the one writer; reused verbatim by the M2 apply loop.
func applyMutation(tx *sql.Tx, m Mutation) error {
	switch m.Op {
	case "insert":
		return insertDocTx(tx, m.Table, m.Doc)
	case "insertMany":
		return insertManyTx(tx, m.Table, m.Doc)
	case "update", "updateMany":
		return updateTx(tx, m)
	case "delete", "deleteMany":
		return deleteTx(tx, m)
	}
	return errors.New("store: unknown mutation op " + m.Op)
}

func insertDocTx(tx *sql.Tx, table string, doc interface{}) error {
	switch table {
	case "users":
		switch v := doc.(type) {
		case User:
			return insertUserTx(tx, v)
		case *User:
			return insertUserTx(tx, *v)
		}
	case "devices":
		switch v := doc.(type) {
		case ConstellationDevice:
			return insertDeviceTx(tx, v)
		case *ConstellationDevice:
			return insertDeviceTx(tx, *v)
		}
	}
	return errors.New("store: unsupported insert doc for table " + table)
}

func insertManyTx(tx *sql.Tx, table string, doc interface{}) error {
	switch v := doc.(type) {
	case []User:
		for _, u := range v {
			if err := insertUserTx(tx, u); err != nil {
				return err
			}
		}
		return nil
	case []ConstellationDevice:
		for _, d := range v {
			if err := insertDeviceTx(tx, d); err != nil {
				return err
			}
		}
		return nil
	}
	return errors.New("store: unsupported insertMany doc for table " + table)
}

func updateTx(tx *sql.Tx, m Mutation) error {
	fields, ok := m.Doc.(map[string]interface{})
	if !ok {
		return errors.New("store: update doc must be a set-fields map")
	}
	if len(fields) == 0 {
		return nil
	}
	cols, err := columnsFor(m.Table)
	if err != nil {
		return err
	}
	keys := sortedKeys(fields)
	set := make([]string, 0, len(keys))
	args := make([]interface{}, 0, len(keys))
	for _, k := range keys {
		col, ok := cols[k]
		if !ok {
			return errors.New("store: unknown field " + k + " for table " + m.Table)
		}
		v, err := toDBValue(fields[k])
		if err != nil {
			return err
		}
		set = append(set, col+" = ?")
		args = append(args, v)
	}
	where, whereArgs, err := buildWhere(m.Table, m.Filter)
	if err != nil {
		return err
	}
	_, err = tx.Exec("UPDATE "+m.Table+" SET "+strings.Join(set, ", ")+where, append(args, whereArgs...)...)
	return err
}

func deleteTx(tx *sql.Tx, m Mutation) error {
	where, args, err := buildWhere(m.Table, m.Filter)
	if err != nil {
		return err
	}
	_, err = tx.Exec("DELETE FROM "+m.Table+where, args...)
	return err
}

func buildWhere(table string, filter map[string]interface{}) (string, []interface{}, error) {
	if len(filter) == 0 {
		return "", nil, nil
	}
	cols, err := columnsFor(table)
	if err != nil {
		return "", nil, err
	}
	keys := sortedKeys(filter)
	clauses := make([]string, 0, len(keys))
	args := make([]interface{}, 0, len(keys))
	for _, k := range keys {
		col, ok := cols[k]
		if !ok {
			return "", nil, errors.New("store: unknown filter field " + k + " for table " + table)
		}
		v, err := toDBValue(filter[k])
		if err != nil {
			return "", nil, err
		}
		clauses = append(clauses, col+" = ?")
		args = append(args, v)
	}
	return " WHERE " + strings.Join(clauses, " AND "), args, nil
}

// bson field name -> column, one map per entity
var userColumns = map[string]string{
	"Nickname":              "nickname",
	"Password":              "password",
	"RegisterKey":           "register_key",
	"RegisterKeyExp":        "register_key_exp",
	"Role":                  "role",
	"PasswordCycle":         "password_cycle",
	"Email":                 "email",
	"RegisteredAt":          "registered_at",
	"LastPasswordChangedAt": "last_password_changed_at",
	"CreatedAt":             "created_at",
	"LastLogin":             "last_login",
	"MFAKey":                "mfa_key",
	"Was2FAVerified":        "was_2fa_verified",
}

var deviceColumns = map[string]string{
	"Nickname":       "nickname",
	"DeviceName":     "device_name",
	"PublicKey":      "public_key",
	"IP":             "ip",
	"IsLighthouse":   "is_lighthouse",
	"CosmosNode":     "cosmos_node",
	"IsRelay":        "is_relay",
	"IsLoadBalancer": "is_load_balancer",
	"IsExitNode":     "is_exit_node",
	"PublicHostname": "public_hostname",
	"Port":           "port",
	"Blocked":        "blocked",
	"Fingerprint":    "fingerprint",
	"APIKey":         "api_key",
	"Invisible":      "invisible",
	"Tags":           "tags",
}

func columnsFor(table string) (map[string]string, error) {
	switch table {
	case "users":
		return userColumns, nil
	case "devices":
		return deviceColumns, nil
	}
	return nil, errors.New("store: unknown table " + table)
}

func sortedKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func toDBValue(v interface{}) (interface{}, error) {
	switch t := v.(type) {
	case nil:
		// fail loudly: '' in an INTEGER column would corrupt the row silently
		return nil, errors.New("store: nil value in mutation")
	case string:
		return t, nil
	case []byte:
		return string(t), nil
	case bool:
		return boolToDB(t), nil
	case int:
		return t, nil
	case int64:
		return t, nil
	case float64:
		return int64(t), nil
	case Role:
		return int(t), nil
	case time.Time:
		return timeToDB(t), nil
	case []string:
		return tagsToDB(t), nil
	}
	return nil, errors.New("store: unsupported value type in mutation")
}

func boolToDB(b bool) int {
	if b {
		return 1
	}
	return 0
}

// time columns: RFC3339Nano UTC TEXT, '' = zero
func timeToDB(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func timeFromDB(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func tagsToDB(tags []string) string {
	if len(tags) == 0 {
		return ""
	}
	data, err := json.Marshal(tags)
	if err != nil {
		return ""
	}
	return string(data)
}

func tagsFromDB(s string) []string {
	if s == "" {
		return nil
	}
	var tags []string
	if err := json.Unmarshal([]byte(s), &tags); err != nil {
		return nil
	}
	return tags
}

// mapSQLError converts a uniqueness rejection into *ErrConstraint; the message
// parse only fills in Table/Index ("devices.device_name"). PRIMARY KEY counts:
// users are keyed by nickname, so a duplicate user arrives as a PK violation and
// must be a rejection like any other, not an error that halts the apply loop.
func mapSQLError(err error) error {
	if err == nil {
		return nil
	}
	var serr *sqlite.Error
	if !errors.As(err, &serr) {
		return err
	}
	if serr.Code() != sqlite3.SQLITE_CONSTRAINT_UNIQUE && serr.Code() != sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY {
		return err
	}
	ec := &ErrConstraint{}
	msg := err.Error()
	marker := "UNIQUE constraint failed: "
	if idx := strings.Index(msg, marker); idx >= 0 {
		target := msg[idx+len(marker):]
		if end := strings.IndexAny(target, " ,("); end > 0 {
			target = target[:end]
		}
		ec.Table = target
		if parts := strings.SplitN(target, ".", 2); len(parts) == 2 {
			ec.Table = parts[0]
			ec.Index = parts[1]
		}
	}
	return ec
}
