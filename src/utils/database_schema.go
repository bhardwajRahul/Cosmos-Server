package utils

import (
	"database/sql"
	"strconv"
	"strings"
)

const monitoringSchemaVersion = 1

// monitoringSchemaV1: node-partitioned monitoring tables. Timestamps are unix
// milliseconds, booleans are 0/1. {{AUTOPK}} and {{FLOAT}} are dialect holes.
const monitoringSchemaV1 = `
CREATE TABLE IF NOT EXISTS monitoring_kv (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS metrics (
	node TEXT NOT NULL,
	key TEXT NOT NULL,
	label TEXT NOT NULL DEFAULT '',
	unit TEXT NOT NULL DEFAULT '',
	max BIGINT NOT NULL DEFAULT 0,
	agglo_type TEXT NOT NULL DEFAULT '',
	scale INTEGER NOT NULL DEFAULT 1,
	object TEXT NOT NULL DEFAULT '',
	time_scale {{FLOAT}} NOT NULL DEFAULT 0,
	last_update BIGINT NOT NULL DEFAULT 0,
	PRIMARY KEY(node, key)
);
CREATE TABLE IF NOT EXISTS metric_values (
	node TEXT NOT NULL,
	key TEXT NOT NULL,
	granularity TEXT NOT NULL,
	date BIGINT NOT NULL,
	value BIGINT NOT NULL DEFAULT 0,
	avg_index INTEGER NOT NULL DEFAULT 0,
	expire BIGINT NOT NULL DEFAULT 0,
	PRIMARY KEY(node, key, granularity, date)
);
CREATE INDEX IF NOT EXISTS idx_metric_values_expire ON metric_values(expire);
CREATE TABLE IF NOT EXISTS events (
	id {{AUTOPK}},
	node TEXT NOT NULL DEFAULT '',
	event_id TEXT NOT NULL DEFAULT '',
	label TEXT NOT NULL DEFAULT '',
	application TEXT NOT NULL DEFAULT '',
	level TEXT NOT NULL DEFAULT '',
	date BIGINT NOT NULL DEFAULT 0,
	data TEXT NOT NULL DEFAULT '',
	object TEXT NOT NULL DEFAULT '',
	search TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_events_date ON events(date);
CREATE INDEX IF NOT EXISTS idx_events_level ON events(level);
CREATE TABLE IF NOT EXISTS notifications (
	id {{AUTOPK}},
	node TEXT NOT NULL DEFAULT '',
	recipient TEXT NOT NULL DEFAULT '',
	title TEXT NOT NULL DEFAULT '',
	message TEXT NOT NULL DEFAULT '',
	vars TEXT NOT NULL DEFAULT '',
	icon TEXT NOT NULL DEFAULT '',
	link TEXT NOT NULL DEFAULT '',
	date BIGINT NOT NULL DEFAULT 0,
	level TEXT NOT NULL DEFAULT '',
	read INTEGER NOT NULL DEFAULT 0,
	actions TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_notifications_recipient ON notifications(recipient, id);
`

func monitoringSchemaFor(dialect string) string {
	autoPK := "INTEGER PRIMARY KEY AUTOINCREMENT"
	float := "REAL"
	if dialect == DialectPostgres {
		autoPK = "BIGSERIAL PRIMARY KEY"
		float = "DOUBLE PRECISION"
	}
	r := strings.NewReplacer("{{AUTOPK}}", autoPK, "{{FLOAT}}", float)
	return r.Replace(monitoringSchemaV1)
}

func initMetricsSchema(db *sql.DB, dialect string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}

	// modernc/sqlite refuses multi-statement Exec, pgx tolerates it: split either way
	for _, stmt := range strings.Split(monitoringSchemaFor(dialect), ";") {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := tx.Exec(stmt); err != nil {
			tx.Rollback()
			return err
		}
	}

	current := 0
	var value string
	err = tx.QueryRow(rebindFor(dialect, "SELECT value FROM monitoring_kv WHERE key = ?"), "schema_version").Scan(&value)
	if err == nil {
		current, _ = strconv.Atoi(value)
	} else if err != sql.ErrNoRows {
		tx.Rollback()
		return err
	}

	// future schema migrations go here, gated on current < N
	_ = current

	set := "INSERT INTO monitoring_kv(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value"
	if _, err := tx.Exec(rebindFor(dialect, set), "schema_version", strconv.Itoa(monitoringSchemaVersion)); err != nil {
		tx.Rollback()
		return err
	}

	return tx.Commit()
}
