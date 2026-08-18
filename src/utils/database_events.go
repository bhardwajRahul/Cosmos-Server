package utils

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// Event is the wire shape the events explorer consumes. Id replaced the Mongo
// ObjectID; the client treats it as an opaque pagination cursor.
type Event struct {
	Id          int64                  `json:"id"`
	Label       string                 `json:"label"`
	Application string                 `json:"application"`
	EventId     string                 `json:"eventId"`
	Date        time.Time              `json:"date"`
	Level       string                 `json:"level"`
	Data        map[string]interface{} `json:"data"`
	Object      string                 `json:"object"`
}

// EventQuery is the parsed form of the events explorer request.
type EventQuery struct {
	From   time.Time
	To     time.Time
	Levels []string
	Search string
	Filter map[string]interface{}
	Cursor int64
	Limit  int
}

// InsertEvent writes one event. Silently a no-op before the store opens (setup runs
// before InitMetricsDatabase and must not error-storm).
func InsertEvent(e Event) error {
	db, err := MonitoringWriteDB()
	if err != nil {
		return nil
	}

	payload, err := json.Marshal(e.Data)
	if err != nil {
		return err
	}

	_, err = db.Exec(MonitoringRebind(
		"INSERT INTO events(node, event_id, label, application, level, date, data, object, search)"+
			" VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)"),
		MonitoringNode(), e.EventId, e.Label, e.Application, e.Level,
		TimeToMillis(e.Date), string(payload), e.Object,
		e.EventId+" "+string(payload))
	return err
}

// QueryEvents returns one page of events plus the total matching count. Reads are
// deliberately not node-scoped: on a shared Postgres, cross-node visibility is the point.
func QueryEvents(q EventQuery) ([]Event, int64, error) {
	db, err := MonitoringReadDB()
	if err != nil {
		return nil, 0, err
	}

	dialect := MonitoringDialect()
	where := []string{"date >= ?", "date <= ?"}
	args := []interface{}{TimeToMillis(q.From), TimeToMillis(q.To)}

	if len(q.Levels) > 0 {
		holes := strings.TrimSuffix(strings.Repeat("?, ", len(q.Levels)), ", ")
		where = append(where, "level IN ("+holes+")")
		for _, l := range q.Levels {
			args = append(args, l)
		}
	}

	if len(q.Filter) > 0 {
		frag, filterArgs, err := TranslateEventFilter(dialect, q.Filter)
		if err != nil {
			return nil, 0, err
		}
		where = append(where, frag)
		args = append(args, filterArgs...)
	} else if q.Search != "" {
		where = append(where, `search LIKE ? ESCAPE '\'`)
		args = append(args, "%"+escapeLike(q.Search)+"%")
	}

	clause := " WHERE " + strings.Join(where, " AND ")

	var total int64
	if err := db.QueryRow(rebindFor(dialect, "SELECT COUNT(*) FROM events"+clause), args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	pageArgs := append([]interface{}{}, args...)
	pageClause := clause
	// a zero cursor is "first page", never a literal id < 0 filter
	if q.Cursor > 0 {
		pageClause += " AND id < ?"
		pageArgs = append(pageArgs, q.Cursor)
	}

	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}

	rows, err := db.Query(rebindFor(dialect,
		"SELECT id, event_id, label, application, level, date, data, object FROM events"+
			pageClause+" ORDER BY date DESC, id DESC LIMIT "+strconv.Itoa(limit)), pageArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	events := []Event{}
	for rows.Next() {
		var e Event
		var date int64
		var payload string
		if err := rows.Scan(&e.Id, &e.EventId, &e.Label, &e.Application, &e.Level, &date, &payload, &e.Object); err != nil {
			return nil, 0, err
		}
		e.Date = MillisToTime(date)
		if payload != "" {
			json.Unmarshal([]byte(payload), &e.Data)
		}
		events = append(events, e)
	}

	return events, total, rows.Err()
}

// DeleteLocalEvents drops this node's events, used by the metrics reset endpoint.
func DeleteLocalEvents() error {
	db, err := MonitoringWriteDB()
	if err != nil {
		return err
	}
	_, err = db.Exec(MonitoringRebind("DELETE FROM events WHERE node = ?"), MonitoringNode())
	return err
}
