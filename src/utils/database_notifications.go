package utils

import (
	"encoding/json"
	"strconv"
	"strings"
)

// InsertNotification writes one already-fanned-out notification row.
func InsertNotification(n Notification) error {
	db, err := MonitoringWriteDB()
	if err != nil {
		return err
	}

	actions, err := json.Marshal(n.Actions)
	if err != nil {
		return err
	}

	read := 0
	if n.Read {
		read = 1
	}

	_, err = db.Exec(MonitoringRebind(
		"INSERT INTO notifications(node, recipient, title, message, vars, icon, link, date, level, read, actions)"+
			" VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)"),
		MonitoringNode(), n.Recipient, n.Title, n.Message, n.Vars, n.Icon, n.Link,
		TimeToMillis(n.Date), n.Level, read, string(actions))
	return err
}

// ListNotifications returns a recipient's newest notifications, keyset-paginated on id.
// Not node-scoped: a shared Postgres should surface every node's notifications.
func ListNotifications(recipient string, cursor int64, limit int) ([]Notification, error) {
	db, err := MonitoringReadDB()
	if err != nil {
		return nil, err
	}

	where := "recipient = ?"
	args := []interface{}{recipient}
	if cursor > 0 {
		where += " AND id < ?"
		args = append(args, cursor)
	}
	if limit <= 0 {
		limit = 20
	}

	rows, err := db.Query(MonitoringRebind(
		"SELECT id, title, message, vars, icon, link, date, level, read, recipient, actions"+
			" FROM notifications WHERE "+where+" ORDER BY id DESC LIMIT "+strconv.Itoa(limit)), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	notifications := []Notification{}
	for rows.Next() {
		var n Notification
		var date int64
		var read int
		var actions string
		if err := rows.Scan(&n.ID, &n.Title, &n.Message, &n.Vars, &n.Icon, &n.Link,
			&date, &n.Level, &read, &n.Recipient, &actions); err != nil {
			return nil, err
		}
		n.Date = MillisToTime(date)
		n.Read = read == 1
		if actions != "" {
			json.Unmarshal([]byte(actions), &n.Actions)
		}
		notifications = append(notifications, n)
	}

	return notifications, rows.Err()
}

// MarkNotificationsRead flips the read flag on the recipient's own rows only.
func MarkNotificationsRead(recipient string, ids []int64) (int64, error) {
	db, err := MonitoringWriteDB()
	if err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}

	holes := strings.TrimSuffix(strings.Repeat("?, ", len(ids)), ", ")
	args := []interface{}{}
	for _, id := range ids {
		args = append(args, id)
	}
	args = append(args, recipient)

	res, err := db.Exec(MonitoringRebind(
		"UPDATE notifications SET read = 1 WHERE id IN ("+holes+") AND recipient = ?"), args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
