package utils

import (
	"database/sql"
	"errors"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

// DialectSQLite / DialectPostgres select DDL and placeholder style.
const (
	DialectSQLite   = "sqlite"
	DialectPostgres = "pgx"
)

var (
	monitorMu      sync.RWMutex
	monitorWriteDB *sql.DB
	monitorReadDB  *sql.DB
	monitorDialect = DialectSQLite
	monitorNode    = "cosmos"
)

// DBStatus reports whether the monitoring store is open, surfaced by /api/status.
var DBStatus bool

var ErrDatabaseClosed = errors.New("database: not initialized")

// InitMetricsDatabase opens the monitoring store (metrics, events, notifications) and
// runs its schema. Deliberately close-then-open so it can be re-invoked when
// CONFIGFOLDER or the Postgres config changes, mirroring InitStore.
func InitMetricsDatabase() error {
	config := GetMainConfig()

	monitorMu.Lock()
	defer monitorMu.Unlock()

	closeMetricsDatabaseLocked()

	monitorNode = resolveNodeName(config)

	dialect := DialectSQLite
	dsn := "file:" + CONFIGFOLDER + "database.db" +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)"

	if config.Database.PostgresHost != "" {
		dialect = DialectPostgres
		dsn = postgresDSN(config.Database)
	}

	w, err := sql.Open(dialect, dsn)
	if err != nil {
		return err
	}
	// SQLite takes a single writer; Postgres handles its own concurrency.
	if dialect == DialectSQLite {
		w.SetMaxOpenConns(1)
	} else {
		w.SetMaxOpenConns(8)
	}

	if err := initMetricsSchema(w, dialect); err != nil {
		w.Close()
		return err
	}

	r, err := sql.Open(dialect, dsn)
	if err != nil {
		w.Close()
		return err
	}
	r.SetMaxOpenConns(4)

	monitorWriteDB = w
	monitorReadDB = r
	monitorDialect = dialect
	DBStatus = true

	Log("Monitoring database ready (" + dialect + ", node " + monitorNode + ")")
	return nil
}

// CloseMetricsDatabase closes both handles; only for shutdown and tests.
func CloseMetricsDatabase() {
	monitorMu.Lock()
	defer monitorMu.Unlock()
	closeMetricsDatabaseLocked()
}

func closeMetricsDatabaseLocked() {
	if monitorWriteDB != nil {
		monitorWriteDB.Close()
		monitorWriteDB = nil
	}
	if monitorReadDB != nil {
		monitorReadDB.Close()
		monitorReadDB = nil
	}
	DBStatus = false
}

// MonitoringWriteDB returns the single-writer handle.
func MonitoringWriteDB() (*sql.DB, error) {
	monitorMu.RLock()
	defer monitorMu.RUnlock()
	if monitorWriteDB == nil {
		return nil, ErrDatabaseClosed
	}
	return monitorWriteDB, nil
}

// MonitoringReadDB returns the pooled reader handle.
func MonitoringReadDB() (*sql.DB, error) {
	monitorMu.RLock()
	defer monitorMu.RUnlock()
	if monitorReadDB == nil {
		return nil, ErrDatabaseClosed
	}
	return monitorReadDB, nil
}

// MonitoringDialect reports which SQL dialect the store speaks.
func MonitoringDialect() string {
	monitorMu.RLock()
	defer monitorMu.RUnlock()
	return monitorDialect
}

// MonitoringNode is the node column value for locally produced rows. Never empty:
// an empty node would recreate the multi-server collision the column exists to prevent.
func MonitoringNode() string {
	monitorMu.RLock()
	defer monitorMu.RUnlock()
	return monitorNode
}

func resolveNodeName(config Config) string {
	if n := strings.TrimSpace(config.Database.NodeName); n != "" {
		return n
	}
	// read straight from config: utils cannot import constellation (import cycle)
	if n := strings.TrimSpace(config.ConstellationConfig.ThisDeviceName); n != "" {
		return n
	}
	if h, err := os.Hostname(); err == nil && strings.TrimSpace(h) != "" {
		return strings.TrimSpace(h)
	}
	return "cosmos"
}

func postgresDSN(db DatabaseConfig) string {
	host, port, err := net.SplitHostPort(db.PostgresHost)
	if err != nil {
		host, port = db.PostgresHost, "5432"
	}

	name := db.PostgresDatabase
	if name == "" {
		name = "cosmos"
	}

	u := &url.URL{
		Scheme: "postgres",
		Host:   net.JoinHostPort(host, port),
		Path:   "/" + name,
	}
	if db.PostgresUsername != "" {
		u.User = url.UserPassword(db.PostgresUsername, db.PostgresPassword)
	}
	return u.String()
}

// Timestamps are stored as INTEGER unix milliseconds everywhere to avoid dialect drift.
func TimeToMillis(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano() / int64(time.Millisecond)
}

func MillisToTime(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.Unix(0, ms*int64(time.Millisecond))
}

func NowMillis() int64 {
	return TimeToMillis(time.Now())
}

// MonitoringRebind turns `?` placeholders into `$n` for Postgres. Queries in this
// package never contain a literal `?` outside a placeholder.
func MonitoringRebind(query string) string {
	return rebindFor(MonitoringDialect(), query)
}

func rebindFor(dialect string, query string) string {
	if dialect != DialectPostgres {
		return query
	}
	var b strings.Builder
	b.Grow(len(query) + 8)
	n := 0
	for i := 0; i < len(query); i++ {
		if query[i] == '?' {
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			continue
		}
		b.WriteByte(query[i])
	}
	return b.String()
}

// escapeLike escapes the LIKE metacharacters in a literal. All LIKE clauses in this
// package are emitted with ESCAPE '\' so both dialects agree.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// WildcardToLike translates a metric key wildcard ("a.b.*") into a LIKE pattern.
// Matching is prefix-anchored, the same as the regex it replaces.
func WildcardToLike(pattern string) string {
	parts := strings.Split(pattern, "*")
	for i, p := range parts {
		parts[i] = escapeLike(p)
	}
	return strings.Join(parts, "%") + "%"
}

// RunDatabaseRetention prunes expired metric buckets and month-old events and
// notifications. Wired to the daily maintenance cron.
func RunDatabaseRetention() {
	db, err := MonitoringWriteDB()
	if err != nil {
		Error("Database Retention", err)
		return
	}

	now := NowMillis()
	monthAgo := now - int64(30*24*time.Hour/time.Millisecond)
	node := MonitoringNode()

	deleted := int64(0)
	for _, q := range []struct {
		sql  string
		args []interface{}
	}{
		{"DELETE FROM metric_values WHERE node = ? AND expire > 0 AND expire < ?", []interface{}{node, now}},
		{"DELETE FROM events WHERE node = ? AND date < ?", []interface{}{node, monthAgo}},
		{"DELETE FROM notifications WHERE node = ? AND date < ?", []interface{}{node, monthAgo}},
	} {
		res, err := db.Exec(MonitoringRebind(q.sql), q.args...)
		if err != nil {
			MajorError("Database Retention", err)
			return
		}
		n, _ := res.RowsAffected()
		deleted += n
	}

	Log("Cleanup: " + strconv.FormatInt(deleted, 10) + " monitoring rows deleted")

	TriggerEvent(
		"cosmos.database.cleanup",
		"Database Cleanup",
		"success",
		"",
		map[string]interface{}{
			"deleted": deleted,
		})
}
