package applier

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/block/pg-sprite/pkg/dbconn"
)

// Snapshot is what the source showed as committed at one instant: which
// transactions a reader taking a snapshot then would see. Drain judges a
// buffered entry only once its transaction is visible in one (CO-4).
//
// The server writes a transaction's commit to the WAL, and waits for any
// synchronous standby to acknowledge it, before it marks the transaction
// visible to new snapshots. The walsender decodes the commit from the WAL
// as soon as it is flushed, so the stream can deliver a change whose
// transaction no reader can see yet. A copier chunk read or a flush's
// completion read taken in that window misses the change; a drain that
// discarded or flushed the entry then would lose it (CO-4). Visibility is
// monotone, so a transaction visible in this snapshot is visible to every
// read taken after it.
//
// The zero Snapshot shows nothing visible, so a drain against one keeps
// every entry: the safe reading when no snapshot was taken.
type Snapshot struct {
	// read is set once the snapshot carries what the server reported.
	read bool
	// all is set by AllVisible: every transaction counts as visible.
	all bool
	// xmin is the lowest transaction still in progress; every transaction
	// below it has completed. xmax is one above the highest completed
	// transaction; every transaction at or above it was still in progress
	// or had not begun. Both are full 64-bit transaction ids, the epoch in
	// the high half.
	xmin, xmax uint64
	// inProgress holds the transactions between xmin and xmax that were
	// still in progress.
	inProgress map[uint64]struct{}
}

// AllVisible returns a snapshot in which every transaction is visible. It
// is for a drain under a lock that excludes every writer of the source:
// each writer's transaction finished, and became visible, before the lock
// was granted.
func AllVisible() Snapshot {
	return Snapshot{read: true, all: true}
}

// ReadSnapshot takes the server's current snapshot on db. The snapshot is
// read as text and parsed here rather than decoded as the server's xid8
// type: the text form is stable across the supported majors.
func ReadSnapshot(ctx context.Context, db dbconn.RowQuerier) (Snapshot, error) {
	var text string
	if err := db.QueryRow(ctx, `SELECT pg_catalog.pg_current_snapshot()::text`).Scan(&text); err != nil {
		return Snapshot{}, fmt.Errorf("read the source's snapshot: %w", err)
	}
	snap, err := parseSnapshot(text)
	if err != nil {
		return Snapshot{}, fmt.Errorf("read the source's snapshot: %w", err)
	}
	return snap, nil
}

// parseSnapshot parses the server's text form of a snapshot:
// xmin:xmax:xip1,xip2,… with the in-progress list possibly empty.
func parseSnapshot(text string) (Snapshot, error) {
	parts := strings.Split(text, ":")
	if len(parts) != 3 {
		return Snapshot{}, fmt.Errorf("%w (CO-4): snapshot %q is not xmin:xmax:xip", ErrInvariantViolation, text)
	}
	xmin, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil {
		return Snapshot{}, fmt.Errorf("%w (CO-4): snapshot %q: xmin: %w", ErrInvariantViolation, text, err)
	}
	xmax, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil {
		return Snapshot{}, fmt.Errorf("%w (CO-4): snapshot %q: xmax: %w", ErrInvariantViolation, text, err)
	}
	if xmin > xmax {
		return Snapshot{}, fmt.Errorf("%w (CO-4): snapshot %q: xmin above xmax", ErrInvariantViolation, text)
	}
	snap := Snapshot{read: true, xmin: xmin, xmax: xmax, inProgress: make(map[uint64]struct{})}
	if parts[2] == "" {
		return snap, nil
	}
	for field := range strings.SplitSeq(parts[2], ",") {
		xid, err := strconv.ParseUint(field, 10, 64)
		if err != nil {
			return Snapshot{}, fmt.Errorf("%w (CO-4): snapshot %q: in-progress transaction: %w", ErrInvariantViolation, text, err)
		}
		snap.inProgress[xid] = struct{}{}
	}
	return snap, nil
}

// Visible reports whether a reader taking this snapshot would see the
// transaction's writes. The stream numbers transactions in 32 bits, as the
// WAL does; the transaction is widened to the 64-bit id nearest the
// snapshot's xmax with those low bits, the way the server itself orders
// 32-bit ids. A transaction at or above xmax is one the server had not
// marked complete when the snapshot was taken, however far the stream has
// delivered it. The zero Snapshot reports nothing visible.
func (s Snapshot) Visible(xid uint32) bool {
	if s.all {
		return true
	}
	if !s.read {
		return false
	}
	full := s.lift(xid)
	if full < s.xmin {
		return true
	}
	if full >= s.xmax {
		return false
	}
	_, inProgress := s.inProgress[full]
	return !inProgress
}

// lift widens a 32-bit transaction id to the 64-bit id nearest xmax whose
// low 32 bits are xid: the signed 32-bit distance from xmax's low bits
// places it within half an epoch either side of xmax, in xmax's epoch, the
// one before, or the one after. A distance that would reach below the
// first id lifts to zero.
func (s Snapshot) lift(xid uint32) uint64 {
	distance := int64(int32(xid - uint32(s.xmax)))
	if distance >= 0 {
		return s.xmax + uint64(distance)
	}
	back := uint64(-distance)
	if back > s.xmax {
		return 0
	}
	return s.xmax - back
}
