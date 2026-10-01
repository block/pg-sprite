package checkpoint

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/progress"
)

const (
	// FormatVersion is the row format Save writes and Load accepts. A row
	// carrying another value was written by an engine whose columns mean
	// something else, and is incompatible whatever its fingerprints say.
	FormatVersion int32 = 1
	// SchemaName is the engine-owned schema the checkpoint table lives in.
	// It is the engine's, not the target's, so one table holds every
	// target's row (D3) and no target schema is written to.
	SchemaName = "pgsprite"
	// TableName is the checkpoint table.
	TableName = "pgsprite_checkpoint"
	// DefaultLoadAttempts bounds how many times Load reads through a
	// transient error before it gives up.
	DefaultLoadAttempts = 5
	// DefaultLoadBackoff is the first wait between Load's attempts; later
	// waits grow linearly.
	DefaultLoadBackoff = 200 * time.Millisecond
)

// SleepFunc waits for d or until ctx is done, returning ctx's error in the
// latter case. Tests inject one that records instead of waiting.
type SleepFunc func(ctx context.Context, d time.Duration) error

// Options bounds the store. Zero values take the defaults: the wall clock,
// DefaultLoadAttempts, DefaultLoadBackoff, and a sleep that waits.
type Options struct {
	// Clock stamps UpdatedAt on every Save.
	Clock progress.Clock
	// LoadAttempts bounds how many reads Load makes through transient
	// errors (ST-2).
	LoadAttempts int
	// LoadBackoff is the wait after the first failed read; the n-th wait is
	// n times it.
	LoadBackoff time.Duration
	// Sleep performs the waits between Load's attempts.
	Sleep SleepFunc
}

func (o Options) withDefaults() Options {
	if o.Clock == nil {
		o.Clock = progress.WallClock{}
	}
	if o.LoadAttempts == 0 {
		o.LoadAttempts = DefaultLoadAttempts
	}
	if o.LoadBackoff == 0 {
		o.LoadBackoff = DefaultLoadBackoff
	}
	if o.Sleep == nil {
		o.Sleep = sleep
	}
	return o
}

// validate runs after withDefaults, so every field is set.
func (o Options) validate() error {
	if o.LoadAttempts < 1 {
		return fmt.Errorf("%w: load attempts %d is below one", ErrInvalidOptions, o.LoadAttempts)
	}
	if o.LoadBackoff < 0 {
		return fmt.Errorf("%w: load backoff %s is negative", ErrInvalidOptions, o.LoadBackoff)
	}
	return nil
}

// sleep is the default SleepFunc.
func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Store persists one Checkpoint per target in the target database (D3). It
// runs over a pkg/dbconn pool, so every statement it issues is already
// under the session's lock_timeout and statement_timeout.
type Store struct {
	pool *pgxpool.Pool
	opts Options
}

// NewStore prepares a store over pool. It refuses options that cannot bound
// a read.
func NewStore(pool *pgxpool.Pool, opts Options) (*Store, error) {
	if pool == nil {
		return nil, fmt.Errorf("%w: pool is nil", ErrInvalidOptions)
	}
	opts = opts.withDefaults()
	if err := opts.validate(); err != nil {
		return nil, err
	}
	return &Store{pool: pool, opts: opts}, nil
}

// tableIdent is the schema-qualified checkpoint table as it appears in
// SQL: SchemaName and TableName quoted, which a unit test pins to what
// pgx.Identifier would render for them. Both names are the engine's own
// constants, never a caller's.
const tableIdent = `"` + SchemaName + `"."` + TableName + `"`

// ensureSQL creates the engine schema and the checkpoint table when they do
// not exist. watermark is NULL while nothing has landed; phase holds the
// stable Phase name so an operator reading the row during an incident sees
// "copying", not a number.
const ensureSQL = "CREATE SCHEMA IF NOT EXISTS \"" + SchemaName + "\";\n" +
	"CREATE TABLE IF NOT EXISTS " + tableIdent + ` (
	schema_name        text NOT NULL,
	table_name         text NOT NULL,
	format_version     integer NOT NULL,
	shadow_table       text NOT NULL,
	slot_name          text NOT NULL,
	publication_name   text NOT NULL,
	watermark          bigint,
	last_applied_lsn   pg_lsn NOT NULL,
	source_fingerprint text NOT NULL,
	target_fingerprint text NOT NULL,
	phase              text NOT NULL,
	updated_at         timestamptz NOT NULL,
	PRIMARY KEY (schema_name, table_name)
)`

// ensureLockSQL takes the engine's own advisory key for the checkpoint
// table — the key pkg/dbconn's table lock would derive for it — for the
// creating transaction, so two engines reaching the same database for the
// first time at once serialize their creates instead of racing CREATE … IF
// NOT EXISTS into a duplicate-key failure in the catalog.
const ensureLockSQL = "SELECT pg_advisory_xact_lock($1, hashtext(quote_ident($2) || '.' || quote_ident($3)))"

// Ensure creates the checkpoint table on first use and is a no-op once it
// exists. The engine role runs it, so the table is the engine's.
func (s *Store) Ensure(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin create of %s: %w", tableIdent, err)
	}
	defer func() {
		// Redundant safety closer: after a successful Commit this returns
		// the guaranteed ErrTxClosed; on a failure path the server aborts
		// the transaction with its session either way.
		_ = tx.Rollback(context.WithoutCancel(ctx))
	}()
	if _, err := tx.Exec(ctx, ensureLockSQL, dbconn.TableLockClassID, SchemaName, TableName); err != nil {
		return fmt.Errorf("lock for create of %s: %w", tableIdent, err)
	}
	if _, err := tx.Exec(ctx, ensureSQL); err != nil {
		return fmt.Errorf("create %s: %w", tableIdent, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit create of %s: %w", tableIdent, err)
	}
	return nil
}

// saveSQL is the one statement Save runs: insert the row, or update the
// existing row for the same target — but only when that row carries this
// run's format version and fingerprints. A row that carries another's is
// left untouched and the statement reports zero rows, which Save turns
// into IncompatibleError (ST-1, ST-2).
const saveSQL = "INSERT INTO " + tableIdent + ` (
	schema_name, table_name, format_version, shadow_table, slot_name, publication_name,
	watermark, last_applied_lsn, source_fingerprint, target_fingerprint, phase, updated_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8::pg_lsn, $9, $10, $11, $12)
ON CONFLICT (schema_name, table_name) DO UPDATE SET
	shadow_table = EXCLUDED.shadow_table,
	slot_name = EXCLUDED.slot_name,
	publication_name = EXCLUDED.publication_name,
	watermark = EXCLUDED.watermark,
	last_applied_lsn = EXCLUDED.last_applied_lsn,
	phase = EXCLUDED.phase,
	updated_at = EXCLUDED.updated_at
WHERE ` + tableIdent + `.format_version = EXCLUDED.format_version
	AND ` + tableIdent + `.source_fingerprint = EXCLUDED.source_fingerprint
	AND ` + tableIdent + `.target_fingerprint = EXCLUDED.target_fingerprint`

// Save writes cp as the target's one row, atomically: a reader sees the
// previous record or this one, never a mix (ST-1). It refuses, with
// IncompatibleError, to write over a row that another statement or another
// row format owns; a run that means to start fresh calls Delete first
// (ST-2). UpdatedAt is taken from the store's clock, not from cp.
func (s *Store) Save(ctx context.Context, cp Checkpoint) error {
	// INV: ST-1
	if err := cp.validate(); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin save of checkpoint for %s.%s: %w", cp.Schema, cp.Table, err)
	}
	defer func() {
		// Redundant safety closer: see Ensure.
		_ = tx.Rollback(context.WithoutCancel(ctx))
	}()
	tag, err := tx.Exec(ctx, saveSQL,
		cp.Schema, cp.Table, FormatVersion, cp.ShadowTable, cp.SlotName, cp.PublicationName,
		watermarkColumn(cp.Watermark), cp.LastAppliedLSN.String(),
		cp.SourceFingerprint, cp.TargetFingerprint, cp.Phase.String(), s.opts.Clock.Now())
	if err != nil {
		return fmt.Errorf("save checkpoint for %s.%s: %w", cp.Schema, cp.Table, err)
	}
	if tag.RowsAffected() == 0 {
		// INV: ST-2
		// The conflicting row is locked by the statement above for the rest
		// of this transaction, so the read sees exactly the row that refused
		// the update.
		stored, err := readRow(ctx, tx, cp.Schema, cp.Table)
		if err != nil {
			return fmt.Errorf("save checkpoint for %s.%s: refused by an existing row that could not be read back: %w", cp.Schema, cp.Table, err)
		}
		if incompatible := stored.incompatibility(cp.Fingerprints()); incompatible != nil {
			return incompatible
		}
		return fmt.Errorf("%w: save of checkpoint for %s.%s updated no row although the existing row is compatible", ErrInvariantViolation, cp.Schema, cp.Table)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit save of checkpoint for %s.%s: %w", cp.Schema, cp.Table, err)
	}
	return nil
}

// watermarkColumn maps the copier's frontier onto the nullable bigint
// column: NULL when nothing has landed.
func watermarkColumn(w copier.Watermark) pgtype.Int8 {
	return pgtype.Int8{Int64: w.Value(), Valid: w.Valid()}
}

// deleteSQL removes the target's row.
const deleteSQL = "DELETE FROM " + tableIdent + " WHERE schema_name = $1 AND table_name = $2"

// Delete removes the target's row so a fresh run can Save its own. It is
// the one deliberate step between an IncompatibleError and a fresh start,
// and it is idempotent: a target with no row is already in the state Delete
// produces.
func (s *Store) Delete(ctx context.Context, schema, table string) error {
	if _, err := s.pool.Exec(ctx, deleteSQL, schema, table); err != nil {
		return fmt.Errorf("delete checkpoint for %s.%s: %w", schema, table, err)
	}
	return nil
}
