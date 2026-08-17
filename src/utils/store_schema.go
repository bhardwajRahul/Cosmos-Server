package utils

import (
	"database/sql"
	"strconv"
)

const storeSchemaVersion = 1

// schemaV1: users keyed by nickname, devices with partial unique indexes on active rows only
// (blocked devices may share name/IP with a live replacement), kv for stamps and versions.
const schemaV1 = `
CREATE TABLE IF NOT EXISTS kv (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS users (
	nickname TEXT PRIMARY KEY,
	password TEXT NOT NULL DEFAULT '',
	register_key TEXT NOT NULL DEFAULT '',
	register_key_exp TEXT NOT NULL DEFAULT '',
	role INTEGER NOT NULL DEFAULT 0,
	password_cycle INTEGER NOT NULL DEFAULT 0,
	email TEXT NOT NULL DEFAULT '',
	registered_at TEXT NOT NULL DEFAULT '',
	last_password_changed_at TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT '',
	last_login TEXT NOT NULL DEFAULT '',
	mfa_key TEXT NOT NULL DEFAULT '',
	was_2fa_verified INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS devices (
	nickname TEXT NOT NULL DEFAULT '',
	device_name TEXT NOT NULL DEFAULT '',
	public_key TEXT NOT NULL DEFAULT '',
	ip TEXT NOT NULL DEFAULT '',
	is_lighthouse INTEGER NOT NULL DEFAULT 0,
	cosmos_node INTEGER NOT NULL DEFAULT 0,
	is_relay INTEGER NOT NULL DEFAULT 0,
	is_load_balancer INTEGER NOT NULL DEFAULT 0,
	is_exit_node INTEGER NOT NULL DEFAULT 0,
	public_hostname TEXT NOT NULL DEFAULT '',
	port TEXT NOT NULL DEFAULT '',
	blocked INTEGER NOT NULL DEFAULT 0,
	fingerprint TEXT NOT NULL DEFAULT '',
	api_key TEXT NOT NULL DEFAULT '',
	invisible INTEGER NOT NULL DEFAULT 0,
	tags TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_devices_active_name ON devices(device_name) WHERE blocked = 0;
CREATE UNIQUE INDEX IF NOT EXISTS idx_devices_active_ip ON devices(ip) WHERE blocked = 0;
`

func initSchema(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}

	if _, err := tx.Exec(schemaV1); err != nil {
		tx.Rollback()
		return err
	}

	current := 0
	value, err := kvGet(tx, "schema_version")
	if err == nil {
		current, _ = strconv.Atoi(value)
	} else if err != ErrNotFound {
		tx.Rollback()
		return err
	}

	// future schema migrations go here, gated on current < N
	_ = current

	if err := kvSetTx(tx, "schema_version", strconv.Itoa(storeSchemaVersion)); err != nil {
		tx.Rollback()
		return err
	}

	return tx.Commit()
}
