package utils

import (
	"database/sql"
)

const deviceCols = "nickname, device_name, public_key, ip, is_lighthouse, cosmos_node, is_relay, is_load_balancer, is_exit_node, public_hostname, port, blocked, fingerprint, api_key, invisible, tags"

func insertDeviceTx(tx *sql.Tx, d ConstellationDevice) error {
	_, err := tx.Exec("INSERT INTO devices ("+deviceCols+") VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		d.Nickname, d.DeviceName, d.PublicKey, d.IP, boolToDB(d.IsLighthouse), d.CosmosNode,
		boolToDB(d.IsRelay), boolToDB(d.IsLoadBalancer), boolToDB(d.IsExitNode), d.PublicHostname,
		d.Port, boolToDB(d.Blocked), d.Fingerprint, d.APIKey, boolToDB(d.Invisible), tagsToDB(d.Tags))
	return err
}

func scanDevices(q rowQuerier, query string, args ...interface{}) ([]ConstellationDevice, error) {
	rows, err := q.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	devices := []ConstellationDevice{}
	for rows.Next() {
		var d ConstellationDevice
		var isLighthouse, isRelay, isLoadBalancer, isExitNode, blocked, invisible int
		var tags string
		if err := rows.Scan(&d.Nickname, &d.DeviceName, &d.PublicKey, &d.IP, &isLighthouse, &d.CosmosNode,
			&isRelay, &isLoadBalancer, &isExitNode, &d.PublicHostname, &d.Port, &blocked,
			&d.Fingerprint, &d.APIKey, &invisible, &tags); err != nil {
			return nil, err
		}
		d.IsLighthouse = isLighthouse != 0
		d.IsRelay = isRelay != 0
		d.IsLoadBalancer = isLoadBalancer != 0
		d.IsExitNode = isExitNode != 0
		d.Blocked = blocked != 0
		d.Invisible = invisible != 0
		d.Tags = tagsFromDB(tags)
		devices = append(devices, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return devices, nil
}

// GetDeviceByName returns one device by name (active only when mustBeActive);
// on any error it returns a zero-value struct, never partial data.
func GetDeviceByName(name string, mustBeActive bool) (ConstellationDevice, error) {
	db, err := getReadDB()
	if err != nil {
		return ConstellationDevice{}, err
	}
	query := "SELECT " + deviceCols + " FROM devices WHERE device_name = ?"
	if mustBeActive {
		query += " AND blocked = 0"
	}
	devices, err := scanDevices(db, query+" LIMIT 1", name)
	if err != nil {
		return ConstellationDevice{}, err
	}
	if len(devices) == 0 {
		return ConstellationDevice{}, ErrNotFound
	}
	return devices[0], nil
}

// GetDeviceByIP returns the active (non-blocked) device holding this IP.
func GetDeviceByIP(ip string) (ConstellationDevice, error) {
	db, err := getReadDB()
	if err != nil {
		return ConstellationDevice{}, err
	}
	devices, err := scanDevices(db, "SELECT "+deviceCols+" FROM devices WHERE ip = ? AND blocked = 0 LIMIT 1", ip)
	if err != nil {
		return ConstellationDevice{}, err
	}
	if len(devices) == 0 {
		return ConstellationDevice{}, ErrNotFound
	}
	return devices[0], nil
}

func ListDevices(includeBlocked bool) ([]ConstellationDevice, error) {
	db, err := getReadDB()
	if err != nil {
		return nil, err
	}
	if includeBlocked {
		return scanDevices(db, "SELECT "+deviceCols+" FROM devices")
	}
	return scanDevices(db, "SELECT "+deviceCols+" FROM devices WHERE blocked = 0")
}

// FindDevices returns devices matching a flat equality filter with bson field names.
func FindDevices(filter map[string]interface{}) ([]ConstellationDevice, error) {
	db, err := getReadDB()
	if err != nil {
		return nil, err
	}
	where, args, err := buildWhere("devices", filter)
	if err != nil {
		return nil, err
	}
	return scanDevices(db, "SELECT "+deviceCols+" FROM devices"+where, args...)
}

func CountDevices(filter map[string]interface{}) (int64, error) {
	db, err := getReadDB()
	if err != nil {
		return 0, err
	}
	where, args, err := buildWhere("devices", filter)
	if err != nil {
		return 0, err
	}
	var count int64
	err = db.QueryRow("SELECT COUNT(*) FROM devices"+where, args...).Scan(&count)
	return count, err
}

func CreateDevice(d ConstellationDevice) error {
	return CommitMutation(Mutation{Table: "devices", Op: "insert", Doc: d})
}

// UpdateDevices sets the given bson-named fields on all devices matching the filter.
func UpdateDevices(filter map[string]interface{}, fields map[string]interface{}) error {
	return CommitMutation(Mutation{Table: "devices", Op: "updateMany", Filter: filter, Doc: fields})
}

func DeleteDevices(filter map[string]interface{}) error {
	return CommitMutation(Mutation{Table: "devices", Op: "deleteMany", Filter: filter})
}

// DeleteDevicesLocal removes devices on this node only, never publishing.
// Reserved for leave/reset semantics — see CommitMutationLocal for why a
// published delete-all is a cluster-wide data loss rather than a local teardown.
func DeleteDevicesLocal(filter map[string]interface{}) error {
	return CommitMutationLocal(Mutation{Table: "devices", Op: "deleteMany", Filter: filter})
}

func dumpDevice(d ConstellationDevice) map[string]interface{} {
	tags := d.Tags
	if tags == nil {
		tags = []string{}
	}
	return map[string]interface{}{
		"Nickname":       d.Nickname,
		"DeviceName":     d.DeviceName,
		"PublicKey":      d.PublicKey,
		"IP":             d.IP,
		"IsLighthouse":   d.IsLighthouse,
		"CosmosNode":     d.CosmosNode,
		"IsRelay":        d.IsRelay,
		"IsLoadBalancer": d.IsLoadBalancer,
		"IsExitNode":     d.IsExitNode,
		"PublicHostname": d.PublicHostname,
		"Port":           d.Port,
		"Blocked":        d.Blocked,
		"Fingerprint":    d.Fingerprint,
		"APIKey":         d.APIKey,
		"Invisible":      d.Invisible,
		"Tags":           tags,
	}
}

func dumpTags(m map[string]interface{}, key string) []string {
	list, ok := m[key].([]interface{})
	if !ok || len(list) == 0 {
		return nil
	}
	tags := make([]string, 0, len(list))
	for _, v := range list {
		if s, ok := v.(string); ok {
			tags = append(tags, s)
		}
	}
	return tags
}

func parseDumpDevice(m map[string]interface{}) ConstellationDevice {
	return ConstellationDevice{
		Nickname:       dumpStr(m, "Nickname"),
		DeviceName:     dumpStr(m, "DeviceName"),
		PublicKey:      dumpStr(m, "PublicKey"),
		IP:             dumpStr(m, "IP"),
		IsLighthouse:   dumpBool(m, "IsLighthouse"),
		CosmosNode:     dumpInt(m, "CosmosNode"),
		IsRelay:        dumpBool(m, "IsRelay"),
		IsLoadBalancer: dumpBool(m, "IsLoadBalancer"),
		IsExitNode:     dumpBool(m, "IsExitNode"),
		PublicHostname: dumpStr(m, "PublicHostname"),
		Port:           dumpStr(m, "Port"),
		Blocked:        dumpBool(m, "Blocked"),
		Fingerprint:    dumpStr(m, "Fingerprint"),
		APIKey:         dumpStr(m, "APIKey"),
		Invisible:      dumpBool(m, "Invisible"),
		Tags:           dumpTags(m, "Tags"),
	}
}
