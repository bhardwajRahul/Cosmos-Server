package utils

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
)

// ErrReadOnly: the op-log publish failed, so this node cannot write config.
var ErrReadOnly = errors.New("store: config is read-only, no writable op-log")

// ErrApplyTimeout: the op was published but did not apply locally in time.
var ErrApplyTimeout = errors.New("store: timed out waiting for the op to apply")

const (
	kvOplogEpoch        = "oplog_epoch"
	kvLastAppliedSeq    = "last_applied_seq"
	kvOplogBootstrapped = "oplog_bootstrapped"
	kvFormationWriter   = "formation_writer"
)

// publishOpHook is the op-log write path; when nil, CommitMutation writes directly.
// Guarded because it is set from constellation Init while HTTP handlers read it.
var publishOpMu sync.RWMutex
var publishOpHook func(Mutation) error

// SetPublishOpHook wires the op-log publisher; nil restores direct writes.
func SetPublishOpHook(h func(Mutation) error) {
	publishOpMu.Lock()
	defer publishOpMu.Unlock()
	publishOpHook = h
}

func getPublishOpHook() func(Mutation) error {
	publishOpMu.RLock()
	defer publishOpMu.RUnlock()
	return publishOpHook
}

type rowScanner interface {
	QueryRow(string, ...interface{}) *sql.Row
}

func kvUint(q rowScanner, key string, def uint64) uint64 {
	value, err := kvGet(q, key)
	if err != nil {
		return def
	}
	n, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return def
	}
	return n
}

// GetOplogEpoch returns the current op-log epoch (1 until a reform bumps it).
func GetOplogEpoch() uint64 {
	db, err := getReadDB()
	if err != nil {
		return 1
	}
	return kvUint(db, kvOplogEpoch, 1)
}

// GetLastAppliedSeq returns the last op-log sequence committed to this node.
func GetLastAppliedSeq() uint64 {
	db, err := getReadDB()
	if err != nil {
		return 0
	}
	return kvUint(db, kvLastAppliedSeq, 0)
}

// IsOplogBootstrapped reports whether this node holds state it materialized from
// the log for the CURRENT epoch — either by installing a snapshot or by seeding
// as founder.
//
// This exists because `last_applied_seq > 0` cannot answer the question. A
// founder that seeded its own store, and every peer that snapshots from it,
// legitimately sit at seq 0 with real rows; seq 0 is therefore not evidence of a
// fresh node. Using it as such both re-triggered the always-snapshot rule forever
// (the joiner never attached) and let an enrolled node fall through to a direct
// write during a bounce (the write forked, never published).
//
// The marker stores the epoch it was set for, so a reform that bumps the epoch
// invalidates it automatically — there is no separate clear path to forget.
func IsOplogBootstrapped() bool {
	db, err := getReadDB()
	if err != nil {
		// fail closed: unknown state must not read as "fresh node, safe to write"
		return true
	}
	return kvUint(db, kvOplogBootstrapped, 0) == kvUint(db, kvOplogEpoch, 1)
}

// MarkOplogBootstrapped records that this node holds valid state for the epoch.
// Snapshot installs set it inside their own tx; this is for the founder-seed path.
func MarkOplogBootstrapped(epoch uint64) error {
	return kvSetOne(kvOplogBootstrapped, strconv.FormatUint(epoch, 10))
}

// IsFormationWriter reports whether this node is the constellation's single
// writer during formation — the creator, or the survivor of a force-reform.
//
// Epoch-tied for the same reason as the bootstrapped marker: the flag stores the
// epoch it was granted for, so a reform that bumps the epoch invalidates any
// older grant automatically and no node can carry a formation licence forward
// into a log it did not seed.
func IsFormationWriter() bool {
	db, err := getReadDB()
	if err != nil {
		// fail closed: an unreadable store must not read as "licensed to write"
		return false
	}
	return kvUint(db, kvFormationWriter, 0) == kvUint(db, kvOplogEpoch, 1)
}

// SetFormationWriter grants the formation write licence for an epoch.
func SetFormationWriter(epoch uint64) error {
	return kvSetOne(kvFormationWriter, strconv.FormatUint(epoch, 10))
}

// ClearFormationWriter ends formation: from here this node publishes like any other.
func ClearFormationWriter() error {
	return kvSetOne(kvFormationWriter, "0")
}

// ReformOplogEpoch re-enters formation on this node in ONE tx: the epoch moves
// past anything the old cluster can publish (its subjects match no stream), the
// sequence restarts, the bootstrapped marker is dropped so peers must
// re-materialize, and this node takes the formation write licence.
func ReformOplogEpoch() (uint64, error) {
	db, err := getWriteDB()
	if err != nil {
		return 0, err
	}
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	epoch := kvUint(tx, kvOplogEpoch, 1) + 1
	if err := setOplogStateTx(tx, epoch, 0); err != nil {
		tx.Rollback()
		return 0, err
	}
	if err := kvSetTx(tx, kvOplogBootstrapped, "0"); err != nil {
		tx.Rollback()
		return 0, err
	}
	if err := kvSetTx(tx, kvFormationWriter, strconv.FormatUint(epoch, 10)); err != nil {
		tx.Rollback()
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return epoch, nil
}

func kvSetOne(key string, value string) error {
	db, err := getWriteDB()
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if err := kvSetTx(tx, key, value); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// SetOplogState overwrites (epoch, seq) and CLEARS the bootstrapped marker; used
// by reform, which deliberately makes the node re-materialize at the new epoch.
func SetOplogState(epoch uint64, seq uint64) error {
	db, err := getWriteDB()
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if err := setOplogStateTx(tx, epoch, seq); err != nil {
		tx.Rollback()
		return err
	}
	// reform: the node must re-materialize at the new epoch before it may write
	if err := kvSetTx(tx, kvOplogBootstrapped, "0"); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func setOplogStateTx(tx *sql.Tx, epoch uint64, seq uint64) error {
	if err := kvSetTx(tx, kvOplogEpoch, strconv.FormatUint(epoch, 10)); err != nil {
		return err
	}
	return kvSetTx(tx, kvLastAppliedSeq, strconv.FormatUint(seq, 10))
}

// ApplyOpTx applies one op-log mutation and commits its sequence in the SAME tx —
// that atomicity, not JetStream ack state, is what makes apply exactly-once.
// preDevices, when non-nil, receives the matching device rows read inside the tx
// (pre-image the apply loop diffs to pick a reaction).
func ApplyOpTx(m Mutation, seq uint64, preDevices *[]ConstellationDevice) error {
	return applyOpTx(m, seq, true, preDevices)
}

// CommitMutationDirect writes one mutation straight to SQLite, still capturing
// the pre-image so the direct write path fires the same reactions as the loop.
func CommitMutationDirect(m Mutation, preDevices *[]ConstellationDevice) error {
	return applyOpTx(m, 0, false, preDevices)
}

// CommitMutationLocal writes to this node ONLY, bypassing the op-log entirely and
// unconditionally — it never consults the write-mode ladder.
//
// RESERVED for leave/teardown semantics, where the intent is inherently local:
// "delete my devices because I am leaving the constellation" must never become a
// cluster op. Routed through CommitMutation instead, a reset's delete-all carries
// an empty filter, which compiles to an unqualified DELETE and wipes the device
// registry on every node that applies it.
//
// Do NOT use for ordinary writes — they must go through CommitMutation so the
// cluster stays a single materialization of one log.
func CommitMutationLocal(m Mutation) error {
	return applyOpTx(m, 0, false, nil)
}

func applyOpTx(m Mutation, seq uint64, commitSeq bool, preDevices *[]ConstellationDevice) error {
	db, err := getWriteDB()
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}

	if preDevices != nil && m.Table == "devices" {
		where, args, errW := buildWhere("devices", m.Filter)
		if errW != nil {
			tx.Rollback()
			return errW
		}
		rows, errQ := scanDevices(tx, "SELECT "+deviceCols+" FROM devices"+where, args...)
		if errQ != nil {
			tx.Rollback()
			return errQ
		}
		*preDevices = rows
	}

	if err := applyMutation(tx, m); err != nil {
		tx.Rollback()
		return mapSQLError(err)
	}
	if commitSeq {
		if err := kvSetTx(tx, kvLastAppliedSeq, strconv.FormatUint(seq, 10)); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// CommitOplogSeq advances last_applied_seq alone — a rejected op still consumes
// its sequence so every replica stays at the same position.
func CommitOplogSeq(seq uint64) error {
	db, err := getWriteDB()
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if err := kvSetTx(tx, kvLastAppliedSeq, strconv.FormatUint(seq, 10)); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// BuildSnapshot returns the users+devices dump plus the (epoch, seq) it is
// consistent with, all read in one tx.
func BuildSnapshot() ([]byte, uint64, uint64, error) {
	db, err := getReadDB()
	if err != nil {
		return nil, 0, 0, err
	}
	tx, err := db.Begin()
	if err != nil {
		return nil, 0, 0, err
	}
	defer tx.Rollback()

	dump, err := buildLogicalDumpTx(tx)
	if err != nil {
		return nil, 0, 0, err
	}
	return dump, kvUint(tx, kvOplogEpoch, 1), kvUint(tx, kvLastAppliedSeq, 0), nil
}

// dbValueKey renders a value in its DB form as a string, so a wire value
// (JSON numbers, DB-normalized strings) compares equal to the same value read
// back out of a row.
func dbValueKey(v interface{}) string {
	dv, err := toDBValue(v)
	if err != nil {
		return ""
	}
	return fmt.Sprint(dv)
}

// DeviceFieldsChanged reports whether an update actually moves any of the named
// fields away from what the matched rows already hold. Lets the apply loop tell
// a real topology move from an edit that merely resubmits the same values.
func DeviceFieldsChanged(pre []ConstellationDevice, fields map[string]interface{}, names map[string]bool) bool {
	for k, v := range fields {
		if !names[k] {
			continue
		}
		want := dbValueKey(v)
		for _, d := range pre {
			if dbValueKey(dumpDevice(d)[k]) != want {
				return true
			}
		}
	}
	return false
}

// HTTPStoreError maps a store/op-log failure onto the status the client expects:
// read-only and duplicate-value both surface as 409, a lost apply as 503.
func HTTPStoreError(w http.ResponseWriter, err error, userCode string) {
	var ec *ErrConstraint
	switch {
	case errors.Is(err, ErrReadOnly):
		HTTPError(w, "Cannot save synchronized settings: the Constellation cluster is unreachable or degraded", http.StatusConflict, userCode)
	case errors.Is(err, ErrApplyTimeout):
		HTTPError(w, "Timed out waiting for the change to be applied", http.StatusServiceUnavailable, userCode)
	case errors.As(err, &ec):
		HTTPError(w, "Duplicate value rejected by "+ec.Table+"."+ec.Index+": already in use", http.StatusConflict, userCode)
	case errors.Is(err, ErrNotFound):
		HTTPError(w, "Not found", http.StatusNotFound, userCode)
	default:
		HTTPError(w, err.Error(), http.StatusInternalServerError, userCode)
	}
}
