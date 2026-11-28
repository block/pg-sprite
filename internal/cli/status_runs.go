package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/decode"
	"github.com/block/pg-sprite/pkg/schemachange"
)

// checkpointTableState is whether the status report could read the
// checkpoint table, and why not when it could not.
type checkpointTableState string

const (
	// checkpointTablePresent: the table was read; stopped_runs lists every
	// row in a non-terminal phase.
	checkpointTablePresent checkpointTableState = "present"
	// checkpointTableAbsent: the database has no checkpoint table, so no
	// copy-and-swap run has ever saved a row in it.
	checkpointTableAbsent checkpointTableState = "absent"
	// checkpointTablePermissionDenied: the role may not read the table, so
	// stopped runs are unknown, not absent.
	checkpointTablePermissionDenied checkpointTableState = "permission-denied"
	// checkpointTableOlderEngine: an older pg-sprite shaped the table and
	// no run of this version has upgraded it yet, so stopped runs are
	// unknown, not absent.
	checkpointTableOlderEngine checkpointTableState = "older-engine"
)

// sqlstateInsufficientPrivilege is the SQLSTATE PostgreSQL raises when the
// role lacks a privilege the statement needs.
const sqlstateInsufficientPrivilege = "42501"

// statusRun is one copy-and-swap run with a checkpoint row in a
// non-terminal phase.
type statusRun struct {
	// Table is the run's table, schema.table as discard and --restart take it.
	Table string `json:"table"`
	// Statement is the SQL the run is carrying out; "" in a row an older
	// engine wrote.
	Statement string `json:"statement"`
	// Phase is the row's phase name.
	Phase string `json:"phase"`
	// UpdatedAt is when the run last saved its row.
	UpdatedAt time.Time `json:"updated_at"`
	// Running is true while a session holds the table's lock, as a run in
	// progress (or a discard) does; RunningPID is that session, 0 when
	// nothing holds it.
	Running    bool   `json:"running"`
	RunningPID uint32 `json:"running_pid"`
	// Slot is the replication slot the row names, null for a run that
	// decodes no WAL.
	Slot *statusSlot `json:"slot"`
	// WriteFence is true when the table carries the write fence, which
	// refuses every write to it.
	WriteFence bool `json:"write_fence"`
}

// stopped reports whether no session is running the run: its leftovers
// stand until someone resumes or discards it.
func (r statusRun) stopped() bool { return !r.Running }

// statusSlot is the replication slot a run's row names, as
// pg_replication_slots reports it.
type statusSlot struct {
	Name string `json:"name"`
	// Exists is false when no logical slot of the name is in this
	// database: dropped by hand or by the server, so a resume makes a
	// new one and reconciles.
	Exists bool `json:"exists"`
	// Active is true while a process holds the slot.
	Active bool `json:"active"`
	// WALStatus is the server's wal_status, "" when the slot does not
	// exist.
	WALStatus string `json:"wal_status"`
	// RetainedBytes is the WAL the slot keeps on the volume,
	// pg_current_wal_lsn() - restart_lsn; null when it does not exist or
	// the server cannot say, as on a standby.
	RetainedBytes *int64 `json:"retained_bytes"`
}

// readStatusRuns lists the runs with a checkpoint row in a non-terminal
// phase, each with what it holds now. A checkpoint table the database does
// not have, the role may not read, or an older engine shaped is reported
// as such with no runs, not as an error.
func readStatusRuns(ctx context.Context, pool *pgxpool.Pool) (checkpointTableState, []statusRun, error) {
	store, err := checkpoint.NewStore(pool, checkpoint.Options{})
	if err != nil {
		return "", nil, err
	}
	rows, err := store.Unfinished(ctx)
	if err != nil {
		state, ok := unreadableCheckpointTable(err)
		if !ok {
			return "", nil, err
		}
		return state, []statusRun{}, nil
	}
	runs := make([]statusRun, 0, len(rows))
	for _, row := range rows {
		run, err := readStatusRun(ctx, pool, row)
		if err != nil {
			return "", nil, err
		}
		runs = append(runs, run)
	}
	return checkpointTablePresent, runs, nil
}

// unreadableCheckpointTable sorts an error from reading the checkpoint
// table into the state the report shows for it, or reports false for an
// error the report cannot stand in for.
func unreadableCheckpointTable(err error) (checkpointTableState, bool) {
	if errors.Is(err, checkpoint.ErrTableMissing) {
		return checkpointTableAbsent, true
	}
	if errors.Is(err, checkpoint.ErrTableBehind) {
		return checkpointTableOlderEngine, true
	}
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == sqlstateInsufficientPrivilege {
		return checkpointTablePermissionDenied, true
	}
	return "", false
}

// readStatusRun reads what the run on row's table holds now: the session
// running it, its slot, and its write fence.
func readStatusRun(ctx context.Context, pool *pgxpool.Pool, row checkpoint.Unfinished) (statusRun, error) {
	run := statusRun{
		Table:     row.Schema + "." + row.Table,
		Statement: row.Statement,
		Phase:     row.Phase,
		UpdatedAt: row.UpdatedAt,
	}
	holder, held, err := dbconn.LookupTableLockHolder(ctx, pool, row.Schema, row.Table)
	if err != nil {
		return statusRun{}, fmt.Errorf("read who runs %s: %w", run.Table, err)
	}
	run.Running, run.RunningPID = held, holder.PID
	if run.WriteFence, err = schemachange.WriteFencePresent(ctx, pool, row.Schema, row.Table); err != nil {
		return statusRun{}, err
	}
	if row.SlotName == "" {
		return run, nil
	}
	slot, found, err := decode.InspectSlot(ctx, pool, row.SlotName)
	if err != nil {
		return statusRun{}, fmt.Errorf("read slot of %s: %w", run.Table, err)
	}
	run.Slot = &statusSlot{Name: row.SlotName}
	if !found || !slot.Own {
		// A slot of the name in another database, or a physical one, is
		// not the one this run decodes from.
		return run, nil
	}
	run.Slot.Exists, run.Slot.Active, run.Slot.WALStatus = true, slot.Active, string(slot.WALStatus)
	if slot.Retained.Known {
		retained := slot.Retained.Bytes
		run.Slot.RetainedBytes = &retained
	}
	return run, nil
}
