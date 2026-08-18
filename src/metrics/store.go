package metrics

import (
	"database/sql"
	"strings"
	"time"

	"github.com/azukaar/cosmos-server/src/utils"
)

const (
	granularityRaw  = "raw"
	granularityHour = "hour"
	granularityDay  = "day"
)

// rawRetention matches what the dashboard renders: raw points older than an hour are
// never read, and the retention cron drops them.
const rawRetention = time.Hour

const catalogUpsert = `INSERT INTO metrics(node, key, label, unit, max, agglo_type, scale, object, time_scale, last_update)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(node, key) DO UPDATE SET label = excluded.label, unit = excluded.unit, max = excluded.max,
agglo_type = excluded.agglo_type, scale = excluded.scale, object = excluded.object,
time_scale = excluded.time_scale, last_update = excluded.last_update`

// The accumulator owns the whole raw bucket, so its row is simply replaced.
const rawUpsert = `INSERT INTO metric_values(node, key, granularity, date, value, avg_index, expire)
VALUES(?, ?, '` + granularityRaw + `', ?, ?, ?, ?)
ON CONFLICT(node, key, granularity, date) DO UPDATE SET
value = excluded.value, avg_index = excluded.avg_index, expire = excluded.expire`

const aggregateInsert = `INSERT INTO metric_values(node, key, granularity, date, value, avg_index, expire)
VALUES(?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(node, key, granularity, date) DO UPDATE SET `

// aggregateMerge expresses the AggloType merge in SQL. sum and avg both accumulate a
// delta so a bucket re-flushed mid-period corrects its own contribution instead of
// double counting; avg divides by avg_index on read.
func aggregateMerge(aggloType string, dialect string) string {
	switch aggloType {
	case "sum", "avg":
		return "value = metric_values.value + ?, avg_index = metric_values.avg_index + ?, expire = excluded.expire"
	case "max":
		fn := "MAX"
		if dialect == utils.DialectPostgres {
			fn = "GREATEST"
		}
		return "value = " + fn + "(metric_values.value, ?), avg_index = metric_values.avg_index + ?, expire = excluded.expire"
	case "min":
		fn := "MIN"
		if dialect == utils.DialectPostgres {
			fn = "LEAST"
		}
		return "value = " + fn + "(metric_values.value, ?), avg_index = metric_values.avg_index + ?, expire = excluded.expire"
	default:
		return "value = ?, avg_index = metric_values.avg_index + ?, expire = excluded.expire"
	}
}

// aggregateArg is the value bound into the merge: a delta for the accumulating types,
// the bucket's current value for the rest.
func aggregateArg(aggloType string, dp DataPush) int {
	switch aggloType {
	case "sum", "avg":
		return dp.Value - dp.Written
	default:
		return dp.Value
	}
}

// writeBuckets persists one flush worth of accumulator entries in a single transaction.
func writeBuckets(buckets []DataPush) error {
	db, err := utils.MonitoringWriteDB()
	if err != nil {
		return err
	}

	dialect := utils.MonitoringDialect()
	node := utils.MonitoringNode()

	tx, err := db.Begin()
	if err != nil {
		return err
	}

	for _, dp := range buckets {
		if err := writeBucket(tx, dialect, node, dp); err != nil {
			tx.Rollback()
			return err
		}
	}

	return tx.Commit()
}

func writeBucket(tx *sql.Tx, dialect string, node string, dp DataPush) error {
	scale := 1
	if dp.Scale != 0 {
		scale = dp.Scale
	}

	_, err := tx.Exec(utils.MonitoringRebind(catalogUpsert),
		node, dp.Key, dp.Label, dp.Unit, int64(dp.Max), dp.AggloType, scale, dp.Object,
		float64(dp.Period/(time.Second*30)), utils.TimeToMillis(dp.Date))
	if err != nil {
		return err
	}

	_, err = tx.Exec(utils.MonitoringRebind(rawUpsert),
		node, dp.Key, utils.TimeToMillis(dp.Date), dp.Value, dp.AvgIndex,
		utils.TimeToMillis(dp.Date.Add(rawRetention)))
	if err != nil {
		return err
	}

	// a bucket contributes one sample to each pool, adjusted in place on re-flush
	countInc := 0
	if !dp.Contributed {
		countInc = 1
	}

	for _, pool := range []struct {
		granularity string
		period      time.Duration
		retention   time.Duration
	}{
		{granularityHour, time.Hour, 48 * time.Hour},
		{granularityDay, 24 * time.Hour, 30 * 24 * time.Hour},
	} {
		poolDate := ModuloTime(dp.Date, pool.period)
		expire := poolDate.Add(pool.period).Add(pool.retention)

		query := aggregateInsert + aggregateMerge(dp.AggloType, dialect)
		_, err = tx.Exec(utils.MonitoringRebind(query),
			node, dp.Key, pool.granularity, utils.TimeToMillis(poolDate),
			dp.Value, 1, utils.TimeToMillis(expire),
			aggregateArg(dp.AggloType, dp), countInc)
		if err != nil {
			return err
		}
	}

	return nil
}

// keyPredicate turns the requested metric list into a parameterized WHERE fragment.
// Wildcards become prefix-anchored LIKE patterns, matching the regex they replace.
func keyPredicate(metricsList []string) (string, []interface{}) {
	if len(metricsList) == 0 {
		return "", nil
	}

	parts := []string{}
	args := []interface{}{}
	for _, metric := range metricsList {
		if strings.Contains(metric, "*") {
			parts = append(parts, `key LIKE ? ESCAPE '\'`)
			args = append(args, utils.WildcardToLike(metric))
		} else {
			parts = append(parts, "key = ?")
			args = append(args, metric)
		}
	}
	return " AND (" + strings.Join(parts, " OR ") + ")", args
}

// loadMetrics rebuilds the DataDefDB documents the dashboard consumes straight from
// SQL. No aggregation happens here: the granularity rows are maintained on write.
func loadMetrics(metricsList []string) ([]DataDefDB, error) {
	db, err := utils.MonitoringReadDB()
	if err != nil {
		return nil, err
	}

	node := utils.MonitoringNode()
	predicate, predicateArgs := keyPredicate(metricsList)

	catalogArgs := append([]interface{}{node}, predicateArgs...)
	rows, err := db.Query(utils.MonitoringRebind(
		"SELECT key, label, unit, max, agglo_type, scale, object, time_scale, last_update"+
			" FROM metrics WHERE node = ?"+predicate+" ORDER BY key"), catalogArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	metrics := []DataDefDB{}
	index := map[string]int{}
	for rows.Next() {
		var m DataDefDB
		var maxValue int64
		var lastUpdate int64
		if err := rows.Scan(&m.Key, &m.Label, &m.Unit, &maxValue, &m.AggloType,
			&m.Scale, &m.Object, &m.TimeScale, &lastUpdate); err != nil {
			return nil, err
		}
		m.Max = uint64(maxValue)
		m.LastUpdate = utils.MillisToTime(lastUpdate)
		m.Values = []DataDefDBEntry{}
		m.ValuesAggl = map[string]DataDefDBEntry{}
		index[m.Key] = len(metrics)
		metrics = append(metrics, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(metrics) == 0 {
		return metrics, nil
	}

	now := utils.NowMillis()
	rawFrom := utils.TimeToMillis(time.Now().Add(-rawRetention))

	valueArgs := append([]interface{}{node}, predicateArgs...)
	valueArgs = append(valueArgs, rawFrom, now)
	vrows, err := db.Query(utils.MonitoringRebind(
		"SELECT key, granularity, date, value, avg_index, expire FROM metric_values"+
			" WHERE node = ?"+predicate+
			" AND ((granularity = '"+granularityRaw+"' AND date >= ?)"+
			" OR (granularity <> '"+granularityRaw+"' AND (expire = 0 OR expire > ?)))"+
			" ORDER BY date"), valueArgs...)
	if err != nil {
		return nil, err
	}
	defer vrows.Close()

	for vrows.Next() {
		var key, granularity string
		var date, value, expire int64
		var avgIndex int
		if err := vrows.Scan(&key, &granularity, &date, &value, &avgIndex, &expire); err != nil {
			return nil, err
		}

		i, ok := index[key]
		if !ok {
			continue
		}

		entry := DataDefDBEntry{
			Date:        utils.MillisToTime(date),
			Value:       int(value),
			AvgIndex:    avgIndex,
			AggloExpire: utils.MillisToTime(expire),
		}

		if granularity == granularityRaw {
			entry.Processed = true
			metrics[i].Values = append(metrics[i].Values, entry)
			continue
		}

		// avg pools accumulate sum + count; the wire value is the average
		if metrics[i].AggloType == "avg" && avgIndex > 0 {
			entry.Value = int(value) / avgIndex
		}
		entry.AggloTo = entry.Date.Add(poolPeriod(granularity))
		metrics[i].ValuesAggl[AggloKey(granularity, entry.Date)] = entry
	}

	return metrics, vrows.Err()
}

func poolPeriod(granularity string) time.Duration {
	if granularity == granularityDay {
		return 24 * time.Hour
	}
	return time.Hour
}

// AggloKey is the ValuesAggl map key the dashboard looks up by literal string.
func AggloKey(granularity string, date time.Time) string {
	if granularity == granularityDay {
		return "day_" + date.UTC().Format("2006-01-02")
	}
	return "hour_" + date.UTC().Format("2006-01-02 15:04:05")
}

// poolValues reads one aggregation pool for every local metric, used for the
// hourly/daily alert sweep when a pool rolls over.
func poolValues(granularity string, poolDate time.Time) (map[string]poolAlertRow, error) {
	db, err := utils.MonitoringReadDB()
	if err != nil {
		return nil, err
	}

	rows, err := db.Query(utils.MonitoringRebind(
		"SELECT m.key, m.object, m.max, m.agglo_type, v.value, v.avg_index"+
			" FROM metric_values v JOIN metrics m ON m.node = v.node AND m.key = v.key"+
			" WHERE v.node = ? AND v.granularity = ? AND v.date = ?"),
		utils.MonitoringNode(), granularity, utils.TimeToMillis(poolDate))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]poolAlertRow{}
	for rows.Next() {
		var r poolAlertRow
		var aggloType string
		var value int64
		var avgIndex int
		if err := rows.Scan(&r.Key, &r.Object, &r.Max, &aggloType, &value, &avgIndex); err != nil {
			return nil, err
		}
		r.Value = int(value)
		if aggloType == "avg" && avgIndex > 0 {
			r.Value = int(value) / avgIndex
		}
		out[r.Key] = r
	}
	return out, rows.Err()
}

type poolAlertRow struct {
	Key    string
	Object string
	Max    uint64
	Value  int
}

func listMetricNames() (map[string]string, error) {
	db, err := utils.MonitoringReadDB()
	if err != nil {
		return nil, err
	}

	rows, err := db.Query(utils.MonitoringRebind(
		"SELECT key, label FROM metrics WHERE node = ?"), utils.MonitoringNode())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	names := map[string]string{}
	for rows.Next() {
		var key, label string
		if err := rows.Scan(&key, &label); err != nil {
			return nil, err
		}
		names[key] = label
	}
	return names, rows.Err()
}

// resetLocalMetrics drops this node's metrics only; a shared Postgres keeps its peers.
func resetLocalMetrics() error {
	db, err := utils.MonitoringWriteDB()
	if err != nil {
		return err
	}

	node := utils.MonitoringNode()
	for _, table := range []string{"metric_values", "metrics"} {
		if _, err := db.Exec(utils.MonitoringRebind("DELETE FROM "+table+" WHERE node = ?"), node); err != nil {
			return err
		}
	}
	return nil
}
