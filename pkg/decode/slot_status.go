package decode

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// WALStatus is pg_replication_slots.wal_status: the server's own verdict on
// whether the WAL a slot still needs is there to be read.
type WALStatus string

const (
	// WALStatusReserved means the slot's WAL is within max_wal_size.
	WALStatusReserved WALStatus = "reserved"
	// WALStatusExtended means the slot holds WAL past max_wal_size, bounded
	// by max_slot_wal_keep_size or unbounded when that is -1.
	WALStatusExtended WALStatus = "extended"
	// WALStatusUnreserved means the slot's WAL is no longer protected from
	// removal and will be lost at the next checkpoint.
	WALStatusUnreserved WALStatus = "unreserved"
	// WALStatusLost means WAL the slot needs has been removed; the slot can
	// never be read again (ST-4 slot loss).
	WALStatusLost WALStatus = "lost"
)

// SlotStatus is one replication slot as pg_replication_slots reports it,
// with the WAL the slot retains measured against the server's current write
// position.
type SlotStatus struct {
	Name string
	// Database owns the slot; the route's own slots are in the pool's
	// current database and only those are ever dropped or reaped.
	Database string
	Logical  bool
	// Active is true while a walsender or a backend holds the slot;
	// ActivePID is that process, zero when inactive.
	Active    bool
	ActivePID int32
	WALStatus WALStatus
	// RestartLSN is the oldest WAL the slot may still need; zero when the
	// server reports none.
	RestartLSN LSN
	// ConfirmedFlushLSN is the position up to which the consumer has
	// confirmed receipt; zero for a physical slot or when unset.
	ConfirmedFlushLSN LSN
	// Retained is the WAL between RestartLSN and the current write
	// position — what the slot keeps on the volume and what D11's lag
	// ceiling bounds.
	Retained RetainedWAL
	// Conflicting is the server's conflict flag for a logical slot
	// (PostgreSQL 16 and later; false before). Before 17 every
	// invalidation sets it; from 17 only a recovery conflict does —
	// Invalidation carries the cause either way.
	Conflicting bool
	// Invalidation is why the server invalidated the slot, when it says.
	Invalidation SlotInvalidation
	// InactiveSince is when the slot was last released by its holder
	// (PostgreSQL 17 and later); zero while held or when unreported.
	InactiveSince time.Time
}

// InvalidationReason is pg_replication_slots.invalidation_reason: the
// server's cause for giving up on a slot.
type InvalidationReason string

const (
	// InvalidationWALRemoved means WAL the slot needed was removed, as
	// when the slot's retention passed max_slot_wal_keep_size.
	InvalidationWALRemoved InvalidationReason = "wal_removed"
	// InvalidationRowsRemoved means rows the slot's catalog snapshot
	// needed were removed on a standby, a recovery conflict.
	InvalidationRowsRemoved InvalidationReason = "rows_removed"
	// InvalidationWALLevelInsufficient means the primary's wal_level fell
	// below logical while a standby held the slot, a recovery conflict.
	InvalidationWALLevelInsufficient InvalidationReason = "wal_level_insufficient"
	// InvalidationIdleTimeout means the slot sat unused past the server's
	// idle slot timeout (PostgreSQL 18 and later).
	InvalidationIdleTimeout InvalidationReason = "idle_timeout"
)

// SlotInvalidation is the server's cause for a lost slot. Reported is
// false on a server that has no invalidation_reason column (before
// PostgreSQL 17), so a cause the server never had is told apart from a
// slot the server has not invalidated — which has Reported true and an
// empty Reason. The lost state (ST-4) is read from WALStatus, never from
// here: the cause only explains it.
type SlotInvalidation struct {
	Reason   InvalidationReason
	Reported bool
}

// String renders the cause for an operator: the server's word when it has
// one, and whether a missing cause is the server's silence or its verdict
// that nothing is wrong.
func (i SlotInvalidation) String() string {
	switch {
	case !i.Reported:
		return "cause not reported by this server version"
	case i.Reason == "":
		return "no invalidation reported"
	default:
		return string(i.Reason)
	}
}

// RetainedWAL is the WAL a slot keeps on the volume. Known is false when
// the server cannot measure it — the slot has no restart position, as
// after the server removed the WAL it needed, or the server is in recovery
// and has no write position to measure against — so an unknown amount is
// never read as nothing retained.
type RetainedWAL struct {
	Bytes int64
	Known bool
}

// inspectSlotSQL reads one slot. The retained measure needs the server's
// write position, which a server in recovery refuses to report, so on a
// standby — a demoted writer the reaper reads after a failover — it is
// NULL and the rest of the row is still read. The conflict flag, the
// invalidation cause, and the inactive-since time exist only from
// PostgreSQL 16, 17, and 17 respectively, so they are read through the
// row's jsonb rendering: a key the server's catalog lacks reads as NULL
// where naming the column would fail the statement, and whether the
// server has the cause column at all is read alongside it.
const inspectSlotSQL = `
	SELECT s.database, s.slot_type = 'logical', s.active, s.active_pid, s.wal_status,
	       s.restart_lsn::text, s.confirmed_flush_lsn::text,
	       CASE WHEN pg_catalog.pg_is_in_recovery() THEN NULL
	            ELSE pg_catalog.pg_wal_lsn_diff(pg_catalog.pg_current_wal_lsn(), s.restart_lsn)::bigint
	       END,
	       (extra.rendered ->> 'conflicting')::boolean,
	       extra.rendered ? 'invalidation_reason',
	       extra.rendered ->> 'invalidation_reason',
	       (extra.rendered ->> 'inactive_since')::timestamptz
	FROM pg_catalog.pg_replication_slots s
	CROSS JOIN LATERAL (SELECT pg_catalog.to_jsonb(s) AS rendered) AS extra
	WHERE s.slot_name = $1`

// InspectSlot reads the named slot from pg_replication_slots on pool. found
// is false when no slot of that name exists on the cluster; a slot of
// another database is reported with its Database so the caller never
// mistakes it for its own. On a server in recovery the slot's retained WAL
// is unknown and the rest of the row is reported. The invalidation cause
// is Reported only on a server that has the column.
func InspectSlot(ctx context.Context, pool *pgxpool.Pool, name string) (status SlotStatus, found bool, err error) {
	var database, walStatus, restartLSN, confirmedFlushLSN, invalidation *string
	var activePID *int32
	var retained *int64
	var conflicting *bool
	var inactiveSince *time.Time
	err = pool.QueryRow(ctx, inspectSlotSQL, name).
		Scan(&database, &status.Logical, &status.Active, &activePID, &walStatus, &restartLSN, &confirmedFlushLSN, &retained,
			&conflicting, &status.Invalidation.Reported, &invalidation, &inactiveSince)
	if errors.Is(err, pgx.ErrNoRows) {
		return SlotStatus{}, false, nil
	}
	if err != nil {
		return SlotStatus{}, false, fmt.Errorf("inspect slot %s: %w", name, err)
	}
	status.Name = name
	if database != nil {
		status.Database = *database
	}
	if activePID != nil {
		status.ActivePID = *activePID
	}
	if walStatus != nil {
		status.WALStatus = WALStatus(*walStatus)
	}
	if conflicting != nil {
		status.Conflicting = *conflicting
	}
	if invalidation != nil {
		status.Invalidation.Reason = InvalidationReason(*invalidation)
	}
	if inactiveSince != nil {
		status.InactiveSince = *inactiveSince
	}
	if restartLSN != nil {
		if status.RestartLSN, err = ParseLSN(*restartLSN); err != nil {
			return SlotStatus{}, false, fmt.Errorf("inspect slot %s: restart_lsn: %w", name, err)
		}
	}
	if confirmedFlushLSN != nil {
		if status.ConfirmedFlushLSN, err = ParseLSN(*confirmedFlushLSN); err != nil {
			return SlotStatus{}, false, fmt.Errorf("inspect slot %s: confirmed_flush_lsn: %w", name, err)
		}
	}
	if retained != nil {
		status.Retained = RetainedWAL{Bytes: *retained, Known: true}
	}
	return status, true, nil
}
