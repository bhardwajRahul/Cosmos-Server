package utils

import (
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"sync"

	_ "modernc.org/sqlite"
)

// ErrNotFound is returned by typed reads when no row matches.
var ErrNotFound = errors.New("store: not found")

// ErrConstraint is returned when a unique index rejects a write.
type ErrConstraint struct {
	Table string
	Index string
}

func (e *ErrConstraint) Error() string {
	return "store: unique constraint violation on " + e.Table + " (" + e.Index + ")"
}

var (
	storeMu sync.RWMutex
	writeDB *sql.DB
	readDB  *sql.DB
)

// InitStore opens auth.db (two handles: single writer, pooled readers) and runs schema migrations.
// Deliberately close-then-open: calling it again re-points the store at the current CONFIGFOLDER
// (tests and the E2E harness rely on this).
func InitStore() error {
	storeMu.Lock()
	defer storeMu.Unlock()

	closeStoreLocked()

	dsn := "file:" + CONFIGFOLDER + "auth.db" +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)"

	w, err := sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	w.SetMaxOpenConns(1)

	if err := initSchema(w); err != nil {
		w.Close()
		return err
	}

	r, err := sql.Open("sqlite", dsn)
	if err != nil {
		w.Close()
		return err
	}
	r.SetMaxOpenConns(4)

	writeDB = w
	readDB = r
	return nil
}

// CloseStore closes both handles; only for shutdown and tests.
func CloseStore() {
	storeMu.Lock()
	defer storeMu.Unlock()
	closeStoreLocked()
}

func closeStoreLocked() {
	if writeDB != nil {
		writeDB.Close()
		writeDB = nil
	}
	if readDB != nil {
		readDB.Close()
		readDB = nil
	}
}

func getWriteDB() (*sql.DB, error) {
	storeMu.RLock()
	defer storeMu.RUnlock()
	if writeDB == nil {
		return nil, errors.New("store: not initialized")
	}
	return writeDB, nil
}

func getReadDB() (*sql.DB, error) {
	storeMu.RLock()
	defer storeMu.RUnlock()
	if readDB == nil {
		return nil, errors.New("store: not initialized")
	}
	return readDB, nil
}

func kvGet(q interface {
	QueryRow(string, ...interface{}) *sql.Row
}, key string) (string, error) {
	var value string
	err := q.QueryRow("SELECT value FROM kv WHERE key = ?", key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", ErrNotFound
	}
	return value, err
}

func kvSetTx(tx *sql.Tx, key string, value string) error {
	_, err := tx.Exec("INSERT INTO kv(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", key, value)
	return err
}

// BuildLogicalDump serializes users+devices as canonical sorted JSON in one read tx.
// Body of the op-log snapshot, and the canonical form E2E convergence asserts on.
func BuildLogicalDump() ([]byte, error) {
	db, err := getReadDB()
	if err != nil {
		return nil, err
	}
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	return buildLogicalDumpTx(tx)
}

func buildLogicalDumpTx(tx *sql.Tx) ([]byte, error) {
	users, err := scanUsers(tx, "SELECT "+userCols+" FROM users")
	if err != nil {
		return nil, err
	}
	devices, err := scanDevices(tx, "SELECT "+deviceCols+" FROM devices")
	if err != nil {
		return nil, err
	}

	sort.Slice(users, func(i, j int) bool { return users[i].Nickname < users[j].Nickname })
	sort.Slice(devices, func(i, j int) bool {
		if devices[i].DeviceName != devices[j].DeviceName {
			return devices[i].DeviceName < devices[j].DeviceName
		}
		if devices[i].IP != devices[j].IP {
			return devices[i].IP < devices[j].IP
		}
		return !devices[i].Blocked && devices[j].Blocked
	})

	userMaps := make([]map[string]interface{}, len(users))
	for i, u := range users {
		userMaps[i] = dumpUser(u)
	}
	deviceMaps := make([]map[string]interface{}, len(devices))
	for i, d := range devices {
		deviceMaps[i] = dumpDevice(d)
	}

	return json.Marshal(map[string]interface{}{
		"users":   userMaps,
		"devices": deviceMaps,
	})
}

// ApplyLogicalDump replaces users+devices with the snapshot content and adopts
// its (epoch, seq) in the same tx, so a crash mid-install can never leave the
// node claiming a log position its data doesn't match.
func ApplyLogicalDump(data []byte, epoch uint64, seq uint64) error {
	var dump struct {
		Users   []map[string]interface{} `json:"users"`
		Devices []map[string]interface{} `json:"devices"`
	}
	if err := json.Unmarshal(data, &dump); err != nil {
		return err
	}

	users := make([]User, len(dump.Users))
	for i, m := range dump.Users {
		users[i] = parseDumpUser(m)
	}
	devices := make([]ConstellationDevice, len(dump.Devices))
	for i, m := range dump.Devices {
		devices[i] = parseDumpDevice(m)
	}

	db, err := getWriteDB()
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM users"); err != nil {
		tx.Rollback()
		return err
	}
	if _, err := tx.Exec("DELETE FROM devices"); err != nil {
		tx.Rollback()
		return err
	}
	for _, u := range users {
		if err := insertUserTx(tx, u); err != nil {
			tx.Rollback()
			return mapSQLError(err)
		}
	}
	for _, d := range devices {
		if err := insertDeviceTx(tx, d); err != nil {
			tx.Rollback()
			return mapSQLError(err)
		}
	}
	if err := setOplogStateTx(tx, epoch, seq); err != nil {
		tx.Rollback()
		return err
	}
	// Bootstrapped in the SAME tx as the rows: a node that holds snapshot data must
	// never be able to look un-bootstrapped afterwards, or it re-snapshots forever
	// (a snapshot from a founder is legitimately stamped seq 0, so the sequence
	// cannot carry this fact).
	if err := kvSetTx(tx, kvOplogBootstrapped, strconv.FormatUint(epoch, 10)); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}
