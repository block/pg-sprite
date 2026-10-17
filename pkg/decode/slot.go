package decode

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
)

// ErrInvariantViolation is the fail-closed sentinel for state this package
// cannot have produced: a zero or non-decoding target, or a slot name that
// is not the engine's.
var ErrInvariantViolation = dbconn.ErrInvariantViolation

// SlotExistsError is returned by CreateSlot when a logical slot of the
// derived name already exists in the target's database. Preflight has
// already ruled out a foreign slot of that name, so this is the route's own
// earlier slot, and whether to resume on it or drop it is the caller's
// decision, never this package's.
type SlotExistsError struct {
	Name string
}

func (e *SlotExistsError) Error() string {
	return fmt.Sprintf("replication slot %s already exists", e.Name)
}

// slotNamePattern is the shape every name this package creates or drops must
// have: the engine's prefix and the eight-hex-digit hash preflight derives.
// A replication command carries the name unquoted, so the shape also keeps
// anything but a bare identifier out of the command.
var slotNamePattern = regexp.MustCompile(`^pgsprite_[0-9a-f]{8}$`)

// SlotNamePrefix is what every engine slot name starts with; a reaper lists
// candidates by it and then asks IsEngineSlotName about each.
const SlotNamePrefix = "pgsprite_"

// IsEngineSlotName reports whether name has the shape this package creates
// and drops. A name that merely wears the prefix is not the engine's and is
// never dropped.
func IsEngineSlotName(name string) bool { return slotNamePattern.MatchString(name) }

// outputPlugin is PostgreSQL's built-in logical-decoding output plugin.
const outputPlugin = "pgoutput"

const (
	sqlstateDuplicateObject   = "42710"
	sqlstateUndefinedObject   = "42704"
	sqlstateInsufficientPrivs = "42501"
	sqlstateObjectInUse       = "55006"
	sqlstateQueryCanceled     = "57014"
)

// slotCommandCancelGrace bounds how long a slot command whose context has
// ended waits for the server to acknowledge the cancel request before its
// connection is cut regardless.
const slotCommandCancelGrace = 5 * time.Second

// endedByContext puts the context's own error into the chain of a slot
// command the server cancelled because that context ended, so the caller
// that ended it finds context.Canceled or context.DeadlineExceeded with
// errors.Is, as it would had the connection been cut instead; any other
// error is returned as it is.
func endedByContext(ctx context.Context, err error) error {
	if ctx.Err() != nil && isSQLState(err, sqlstateQueryCanceled) {
		return fmt.Errorf("%w: %w", ctx.Err(), err)
	}
	return err
}

// Slot is a logical replication slot this package created, with the
// replication connection that created it still open. The connection is kept
// because the exported snapshot lives only as long as the walsender's
// transaction, which ends with the connection's next command or its close;
// a caller that wants to copy from the slot's own snapshot imports it (SET
// TRANSACTION SNAPSHOT) before either happens. The slot itself is durable
// and outlives Close.
type Slot struct {
	name            string
	consistentPoint LSN
	snapshotName    string
	conn            *pgconn.PgConn
}

// Name is the slot's name, which is also the publication's.
func (s *Slot) Name() string { return s.name }

// ConsistentPoint is the LSN from which decoding on this slot yields every
// change not already visible in the exported snapshot.
func (s *Slot) ConsistentPoint() LSN { return s.consistentPoint }

// SnapshotName is the exported snapshot's name, importable by another
// session of the same database while the replication connection's walsender
// runs.
func (s *Slot) SnapshotName() string { return s.snapshotName }

// Close ends the replication connection. The server ends the exported
// snapshot when the connection's walsender exits, which is after Close
// returns: a caller that needs the snapshot gone waits for that backend to
// leave pg_stat_activity. The slot persists.
func (s *Slot) Close(ctx context.Context) error {
	if err := s.conn.Close(ctx); err != nil {
		return fmt.Errorf("close replication connection for slot %s: %w", s.name, err)
	}
	return nil
}

// CreateSlot creates the single-table publication and the logical
// replication slot for a copy-and-swap target, both under the name preflight
// derived for it, and returns the slot with its exported snapshot and
// consistent point. The publication is created on pool first — a publication
// the engine already created for exactly this table is reused, one that
// publishes anything else or anything less is refused as a
// *ForeignStateError — so a privilege refusal (*PublicationPrivilegeError)
// leaves no slot behind. The slot is created on a fresh replication
// connection built from cfg, proven to be on the pool's own cluster and
// database before the slot exists anywhere; the connection stays open inside
// the returned Slot. A logical slot of the derived name already present in
// the target's database is reported as a *SlotExistsError.
//
// No error leaves a slot behind: a refusal before the create makes none, a
// create the caller's context ends is cancelled on the server — the slot
// under construction goes with its walsender, and the error carries the
// context's own error — and a slot the server created but did not describe usably is
// dropped before the error is returned. The publication is left in place
// between the two — it is the route's own and the next attempt reuses it.
// A run calls it under the engine's lock on the slot's name
// (checkpoint.Store.WithSlotLock), the same lock the reaper holds while it
// judges a slot, so a slot with no row is one the reaper may drop only once
// the run's claim has ended: an orphan, never a slot mid-claim.
func CreateSlot(ctx context.Context, cfg dbconn.Config, pool *pgxpool.Pool, target preflight.CopySwapTarget) (*Slot, error) {
	if target.Table() == "" {
		return nil, fmt.Errorf("%w: ST-3: zero copy-and-swap target", ErrInvariantViolation)
	}
	if !target.DecodesWAL() {
		return nil, fmt.Errorf("%w: ST-3: the target's environment was not verified for a run that decodes WAL", ErrInvariantViolation)
	}
	name := target.DecodingName()
	if !slotNamePattern.MatchString(name) {
		return nil, fmt.Errorf("%w: ST-3: derived name %q is not an engine slot name", ErrInvariantViolation, name)
	}
	if err := ensurePublication(ctx, pool, name, target); err != nil {
		return nil, err
	}

	conn, err := dbconn.ConnectReplication(ctx, cfg, dbconn.WithServerSideCancel(slotCommandCancelGrace))
	if err != nil {
		return nil, fmt.Errorf("create slot %s: %w", name, err)
	}
	// The pool was proven to be on the target's database when the
	// publication was made; proving the connection is on the pool's cluster
	// and database puts the slot where the name says.
	if _, err := proveSameServer(ctx, conn, pool); err != nil {
		return nil, errors.Join(fmt.Errorf("create slot %s: %w", name, err), conn.Close(ctx))
	}

	result, err := pglogrepl.CreateReplicationSlot(ctx, conn, name, outputPlugin, pglogrepl.CreateReplicationSlotOptions{
		Mode:           pglogrepl.LogicalReplication,
		SnapshotAction: "EXPORT_SNAPSHOT",
	})
	if err != nil {
		closeErr := conn.Close(context.WithoutCancel(ctx))
		if isSQLState(err, sqlstateDuplicateObject) {
			return nil, errors.Join(&SlotExistsError{Name: name}, closeErr)
		}
		return nil, errors.Join(fmt.Errorf("create slot %s: %w", name, endedByContext(ctx, err)), closeErr)
	}
	consistentPoint, err := ParseLSN(result.ConsistentPoint)
	if err != nil {
		return nil, dropUnusableSlot(ctx, conn, name, fmt.Errorf("create slot %s: consistent point: %w", name, err))
	}
	if result.SnapshotName == "" {
		return nil, dropUnusableSlot(ctx, conn, name,
			fmt.Errorf("%w: ST-3: slot %s was created without an exported snapshot", ErrInvariantViolation, name))
	}
	return &Slot{name: name, consistentPoint: consistentPoint, snapshotName: result.SnapshotName, conn: conn}, nil
}

// dropUnusableSlot drops a slot the server has just created but described
// in a way this package cannot hand back, on the connection that created
// it, so the cause is returned with no slot behind it; the connection is
// closed whatever the outcome. Nothing streams from a slot this new, so the
// drop does not wait.
func dropUnusableSlot(ctx context.Context, conn *pgconn.PgConn, name string, cause error) error {
	// INV: ST-3 — a CreateSlot error never leaves a slot to reap.
	dropErr := pglogrepl.DropReplicationSlot(ctx, conn, name, pglogrepl.DropReplicationSlotOptions{})
	if dropErr != nil {
		dropErr = fmt.Errorf("drop unusable slot %s: %w", name, dropErr)
	}
	return errors.Join(cause, dropErr, conn.Close(ctx))
}

// isSQLState reports whether err is a PostgreSQL error carrying the SQLSTATE.
func isSQLState(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}
