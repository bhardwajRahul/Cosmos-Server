package utils

import (
	"encoding/json"
	"errors"
)

// The op-log wire form deliberately does NOT reuse the structs' json tags:
// User.Password, ConstellationDevice.APIKey and friends are `json:"-"` and would
// be dropped. Docs travel as the bson-named dump maps instead, and set-field maps
// travel pre-converted to their DB representation so every replica writes bytes
// identical to the originating node's.

// EncodeOpDoc renders a mutation's Doc into its canonical wire form.
func EncodeOpDoc(m Mutation) (json.RawMessage, error) {
	switch m.Op {
	case "insert":
		doc, err := dumpDoc(m.Table, m.Doc)
		if err != nil {
			return nil, err
		}
		return json.Marshal(doc)
	case "insertMany":
		docs, err := dumpDocs(m.Table, m.Doc)
		if err != nil {
			return nil, err
		}
		return json.Marshal(docs)
	case "update", "updateMany":
		fields, ok := m.Doc.(map[string]interface{})
		if !ok {
			return nil, errors.New("store: update doc must be a set-fields map")
		}
		norm, err := NormalizeOpFields(m.Table, fields)
		if err != nil {
			return nil, err
		}
		return json.Marshal(norm)
	case "delete", "deleteMany":
		return nil, nil
	}
	return nil, errors.New("store: unknown mutation op " + m.Op)
}

// DecodeOpDoc rebuilds a Doc from the wire form for this table and op.
func DecodeOpDoc(table string, op string, raw json.RawMessage) (interface{}, error) {
	switch op {
	case "insert":
		var m map[string]interface{}
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, err
		}
		switch table {
		case "users":
			return parseDumpUser(m), nil
		case "devices":
			return parseDumpDevice(m), nil
		}
	case "insertMany":
		var list []map[string]interface{}
		if err := json.Unmarshal(raw, &list); err != nil {
			return nil, err
		}
		switch table {
		case "users":
			users := make([]User, len(list))
			for i, m := range list {
				users[i] = parseDumpUser(m)
			}
			return users, nil
		case "devices":
			devices := make([]ConstellationDevice, len(list))
			for i, m := range list {
				devices[i] = parseDumpDevice(m)
			}
			return devices, nil
		}
	case "update", "updateMany":
		var m map[string]interface{}
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, err
		}
		return m, nil
	case "delete", "deleteMany":
		return nil, nil
	default:
		return nil, errors.New("store: unknown mutation op " + op)
	}
	return nil, errors.New("store: unsupported doc for table " + table)
}

// NormalizeOpFields converts a set-fields map to its DB representation, rejecting
// unknown field names before they ever reach the log.
func NormalizeOpFields(table string, fields map[string]interface{}) (map[string]interface{}, error) {
	cols, err := columnsFor(table)
	if err != nil {
		return nil, err
	}
	out := make(map[string]interface{}, len(fields))
	for k, v := range fields {
		if _, ok := cols[k]; !ok {
			return nil, errors.New("store: unknown field " + k + " for table " + table)
		}
		dbv, err := toDBValue(v)
		if err != nil {
			return nil, err
		}
		out[k] = dbv
	}
	return out, nil
}

// NormalizeOpFilter does the same for a filter's equality values.
func NormalizeOpFilter(table string, filter map[string]interface{}) (map[string]interface{}, error) {
	if filter == nil {
		return nil, nil
	}
	return NormalizeOpFields(table, filter)
}

func dumpDoc(table string, doc interface{}) (map[string]interface{}, error) {
	switch table {
	case "users":
		switch v := doc.(type) {
		case User:
			return dumpUser(v), nil
		case *User:
			return dumpUser(*v), nil
		}
	case "devices":
		switch v := doc.(type) {
		case ConstellationDevice:
			return dumpDevice(v), nil
		case *ConstellationDevice:
			return dumpDevice(*v), nil
		}
	}
	return nil, errors.New("store: unsupported doc for table " + table)
}

func dumpDocs(table string, doc interface{}) ([]map[string]interface{}, error) {
	switch v := doc.(type) {
	case []User:
		out := make([]map[string]interface{}, len(v))
		for i, u := range v {
			out[i] = dumpUser(u)
		}
		return out, nil
	case []ConstellationDevice:
		out := make([]map[string]interface{}, len(v))
		for i, d := range v {
			out[i] = dumpDevice(d)
		}
		return out, nil
	}
	return nil, errors.New("store: unsupported insertMany doc for table " + table)
}
