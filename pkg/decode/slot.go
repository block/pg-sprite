package decode

import (
	"context"
	"errors"
	"fmt"
	"regexp"

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

// ErrForeignDecodingState is returned when the publication or the slot of
// the derived name turns out not to be this route's: a publication that
// publishes anything but the target table, or a slot another database owns.
// Neither is ever adopted or dropped; the operator resolves it by renaming
// or dropping the foreign object.
var ErrForeignDecodingState = errors.New("logical-decoding state of the derived name is not this route's")

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

// outputPlugin is PostgreSQL's built-in logical-decoding output plugin.
const outputPlugin = "pgoutput"

const (
	sqlstateDuplicateObject   = "42710"
	sqlstateUndefinedObject   = "42704"
	sqlstateInsufficientPrivs = "42501"
)

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
// session of the same database until Close.
func (s *Slot) SnapshotName() string { return s.snapshotName }

// Close ends the replication connection, and with it the exported snapshot.
// The slot persists.
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
// the engine already created for exactly this table is reused — so a
// privilege refusal (CREATE on the database) leaves no slot behind. The slot
// is created on a fresh replication connection built from cfg, which must
// reach the target's database; the connection stays open inside the returned
// Slot. A logical slot of the derived name already present in the target's
// database is reported as a *SlotExistsError.
//
// The caller has committed the target's checkpoint row before calling: a
// slot with no row is an orphan for the reaper, never a slot mid-creation.
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

	conn, err := dbconn.ConnectReplication(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create slot %s: %w", name, err)
	}
	// INV: ST-3 — the slot is named for the target's database and must be
	// created there; cfg is the caller's and is proven against the target
	// before a slot of that name exists anywhere else.
	identity, err := pglogrepl.IdentifySystem(ctx, conn)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("identify replication system for slot %s: %w", name, err), conn.Close(ctx))
	}
	if identity.DBName != target.Database() {
		return nil, errors.Join(fmt.Errorf("%w: ST-3: replication connection is on database %q, the target is in %q",
			ErrInvariantViolation, identity.DBName, target.Database()), conn.Close(ctx))
	}

	result, err := pglogrepl.CreateReplicationSlot(ctx, conn, name, outputPlugin, pglogrepl.CreateReplicationSlotOptions{
		Mode:           pglogrepl.LogicalReplication,
		SnapshotAction: "EXPORT_SNAPSHOT",
	})
	if err != nil {
		closeErr := conn.Close(ctx)
		if isSQLState(err, sqlstateDuplicateObject) {
			return nil, errors.Join(&SlotExistsError{Name: name}, closeErr)
		}
		return nil, errors.Join(fmt.Errorf("create slot %s: %w", name, err), closeErr)
	}
	consistentPoint, err := ParseLSN(result.ConsistentPoint)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("create slot %s: consistent point: %w", name, err), conn.Close(ctx))
	}
	if result.SnapshotName == "" {
		return nil, errors.Join(fmt.Errorf("%w: ST-3: slot %s was created without an exported snapshot", ErrInvariantViolation, name), conn.Close(ctx))
	}
	return &Slot{name: name, consistentPoint: consistentPoint, snapshotName: result.SnapshotName, conn: conn}, nil
}

// isSQLState reports whether err is a PostgreSQL error carrying the SQLSTATE.
func isSQLState(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}
