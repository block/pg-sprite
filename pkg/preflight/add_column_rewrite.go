package preflight

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/statement"
)

// ErrAddColumnProbeUnavailable means the server refused the temporary
// table the add-column proof adds each column to: the server accepts no
// writes (a hot standby, or default_transaction_read_only), or the role
// lacks TEMPORARY on the database. Without the proof the plan cannot say
// whether adding the column rewrites the table, so it fails rather than
// guess.
var ErrAddColumnProbeUnavailable = errors.New("the add-column proof cannot create its temporary table")

// AddColumnRewrites reports, for each column the ALTER TABLE in sql adds to
// schema.table, what the server proved about adding it: whether it
// rewrites the table, and if so whether the column's default or its type
// is the cause. The server answers, not the statement's shape: a constant
// DEFAULT still rewrites every row when the column's type is a domain with
// constraints, and a DEFAULT now() does not, because the server stores any
// non-volatile default in the catalog.
//
// Each column is added on its own (statement.AddColumnProbes) to an empty
// temporary table with no columns, inside a savepoint of a transaction that
// is always rolled back, and a rewrite shows as the probe table's storage
// being replaced (pg_relation_filenode changes). The probe rewrites exactly
// when schema.table would, since the decision depends on the added column's
// type and default, never on the table's rows or its other columns: a
// default cannot name a column, and a generated column, whose expression
// can, gets no probe. A column that rewrites is probed again with its type
// alone, as the server resolved it and with no default or identity, which
// tells a rewriting type from a rewriting default. The live table is never
// opened by the probe itself, and needs no privilege.
//
// Adding the column still evaluates its default once, and its domain's
// checks, the way the real ALTER does, and a function either one calls can
// read any table, the live one included. A probe never waits for a lock:
// one that cannot be granted at once proves nothing, and the column is
// reported as statement.AddColumnUnproven. Each probe's savepoint is rolled
// back as soon as it is read, which releases whatever it locked.
//
// An ADD COLUMN IF NOT EXISTS for a column the table already has is skipped
// by the server, so it gets no proof and the column is absent from the
// result. A server that refuses the probe table returns
// ErrAddColumnProbeUnavailable. A probe that fails for any other reason,
// such as a column type that does not exist, is returned as an error,
// never read as an answer.
func AddColumnRewrites(ctx context.Context, pool *pgxpool.Pool, schema, table, sql string) (map[string]statement.AddColumnProof, error) {
	probeTable, err := probeTableName()
	if err != nil {
		return nil, err
	}
	probes, err := statement.AddColumnProbes(sql, "pg_temp", probeTable)
	if err != nil {
		return nil, fmt.Errorf("build add-column probes for %s: %w", qualifiedName(schema, table), err)
	}
	if len(probes) == 0 {
		return nil, nil
	}
	st, err := statement.ParseOne(sql)
	if err != nil {
		return nil, fmt.Errorf("parse add-column statement for %s: %w", qualifiedName(schema, table), err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin add-column probe for %s: %w", qualifiedName(schema, table), err)
	}
	// The probe transaction is never committed: rollback is the cleanup on
	// success and failure alike, and drops the temporary table with it.
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()

	// The probes resolve the statement's unqualified names, a column's
	// type above all, on the path the native executor runs it under: the
	// statement's own schema then public, or the session's path when the
	// statement names no schema. A lock_timeout of one millisecond, the
	// smallest that is not "wait for ever", keeps a probe from queueing
	// behind another session's lock.
	setup := "SET LOCAL lock_timeout = 1"
	if st.Schema() != "" {
		setup += "; " + dbconn.LocalSearchPath(st.Schema(), "public")
	}
	if _, err := tx.Exec(ctx, setup); err != nil {
		return nil, fmt.Errorf("set up the add-column probe for %s: %w", qualifiedName(schema, table), err)
	}
	existing, err := existingColumns(ctx, tx, schema, table, probes)
	if err != nil {
		return nil, err
	}

	proofs := make(map[string]statement.AddColumnProof, len(probes))
	for column, probe := range probes {
		if probe.IfNotExists && existing[column] {
			continue
		}
		proofs[column], err = proveAddColumn(ctx, tx, probeTable, column, probe.SQL)
		if err != nil {
			return nil, fmt.Errorf("add column %q of %s to the probe table: %w", column, qualifiedName(schema, table), err)
		}
	}
	if err := tx.Rollback(ctx); err != nil {
		return nil, fmt.Errorf("roll back add-column probe for %s: %w", qualifiedName(schema, table), err)
	}
	return proofs, nil
}

// SQLSTATEs the probe tells apart, beside insufficient_privilege:
// lock_not_available, raised when a lock is not granted within
// lock_timeout, and read_only_sql_transaction, raised by a hot standby or
// a read-only transaction.
const (
	sqlstateLockNotAvailable       = "55P03"
	sqlstateReadOnlySQLTransaction = "25006"
)

// errProbeLockNotAvailable means a probe needed a lock it could not take
// at once, so it proved nothing.
var errProbeLockNotAvailable = errors.New("the probe could not take a lock at once")

// proveAddColumn reports what the server proves about adding one column:
// the probe adds it as the statement does, and when that rewrites, a
// second probe adds the column's type alone to tell which one is the
// cause. A probe that cannot take a lock at once leaves the column
// unproven.
func proveAddColumn(ctx context.Context, tx pgx.Tx, probeTable, column, probe string) (statement.AddColumnProof, error) {
	rewrites, typeName, err := probeRewrite(ctx, tx, probeTable, column, func(string) string { return probe })
	switch {
	case errors.Is(err, errProbeLockNotAvailable):
		return statement.AddColumnUnproven, nil
	case err != nil:
		return "", err
	case !rewrites:
		return statement.AddColumnInPlace, nil
	}
	// typeName is format_type's rendering of the type the server resolved
	// for the column, quoted and qualified as needed for the probe's
	// search_path, so it names the same type here. A serial or identity
	// column reads back as its integer type, without the default its
	// sequence supplies.
	typeRewrites, _, err := probeRewrite(ctx, tx, probeTable, column, func(probeIdent string) string {
		return "ALTER TABLE " + probeIdent + " ADD COLUMN " + pgx.Identifier{column}.Sanitize() + " " + typeName
	})
	switch {
	case errors.Is(err, errProbeLockNotAvailable):
		return statement.AddColumnUnproven, nil
	case err != nil:
		return "", fmt.Errorf("add the type of column %q alone: %w", column, err)
	case typeRewrites:
		return statement.AddColumnTypeRewrites, nil
	default:
		return statement.AddColumnDefaultRewrites, nil
	}
}

// probeRewrite runs the ALTER that alter builds for the probe table's
// qualified name against a fresh probe table, inside a savepoint, and
// reports whether it replaced the table's storage, with the type the
// server resolved for column. A lock the probe cannot take at once is
// errProbeLockNotAvailable. The savepoint is rolled back either way,
// dropping the probe table and releasing its locks.
func probeRewrite(ctx context.Context, tx pgx.Tx, probeTable, column string, alter func(probeIdent string) string) (rewrites bool, typeName string, err error) {
	probeIdent := pgx.Identifier{"pg_temp", probeTable}.Sanitize()
	if _, err := tx.Exec(ctx, "SAVEPOINT pgsprite_probe"); err != nil {
		return false, "", fmt.Errorf("open the probe savepoint: %w", err)
	}
	if _, err := tx.Exec(ctx, "CREATE TEMPORARY TABLE "+pgx.Identifier{probeTable}.Sanitize()+" ()"); err != nil {
		return false, "", probeTableError(err)
	}
	var before, after uint32
	if err := tx.QueryRow(ctx, "SELECT pg_relation_filenode($1::regclass)", probeIdent).Scan(&before); err != nil {
		return false, "", fmt.Errorf("read the probe table's storage before adding column %q: %w", column, err)
	}
	_, err = tx.Exec(ctx, alter(probeIdent))
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &pgErr) && pgErr.Code == sqlstateLockNotAvailable:
		err = errProbeLockNotAvailable
	case err != nil:
		return false, "", err
	default:
		if err := tx.QueryRow(ctx, `SELECT pg_relation_filenode($1::regclass),
			(SELECT format_type(atttypid, atttypmod) FROM pg_catalog.pg_attribute
			 WHERE attrelid = $1::regclass AND attname = $2)`, probeIdent, column).Scan(&after, &typeName); err != nil {
			return false, "", fmt.Errorf("read the probe table's storage after adding column %q: %w", column, err)
		}
		rewrites = before != after
	}
	if _, rollbackErr := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT pgsprite_probe"); rollbackErr != nil {
		return false, "", fmt.Errorf("roll back the probe savepoint: %w", rollbackErr)
	}
	return rewrites, typeName, err
}

// probeTableError separates the server refusing the probe table, which
// only the deployment can fix, from any other failure to create it.
func probeTableError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case sqlstateReadOnlySQLTransaction:
			return fmt.Errorf("%w: the server accepts no writes; plan the change against the primary: %w", ErrAddColumnProbeUnavailable, err)
		case sqlstateInsufficientPrivilege:
			return fmt.Errorf("%w: grant TEMPORARY on the database to the role pg-sprite connects as: %w", ErrAddColumnProbeUnavailable, err)
		}
	}
	return fmt.Errorf("create the probe table: %w", err)
}

// existingColumns reads which of the columns an IF NOT EXISTS probe adds
// schema.table already has, from the catalog alone: it takes no lock on
// the table.
func existingColumns(ctx context.Context, tx pgx.Tx, schema, table string, probes map[string]statement.AddColumnProbe) (map[string]bool, error) {
	var names []string
	for column, probe := range probes {
		if probe.IfNotExists {
			names = append(names, column)
		}
	}
	if len(names) == 0 {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `SELECT a.attname FROM pg_catalog.pg_attribute a
		JOIN pg_catalog.pg_class c ON c.oid = a.attrelid
		JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2 AND a.attnum > 0 AND NOT a.attisdropped
		  AND a.attname = ANY($3)`, schema, table, names)
	if err != nil {
		return nil, fmt.Errorf("read the existing columns of %s: %w", qualifiedName(schema, table), err)
	}
	existing, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read the existing columns of %s: %w", qualifiedName(schema, table), err)
	}
	out := make(map[string]bool, len(existing))
	for _, name := range existing {
		out[name] = true
	}
	return out, nil
}

// probeTableName returns a temporary table name no other object in the
// session's temporary schema has. The table never outlives its
// transaction.
func probeTableName() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate probe table name: %w", err)
	}
	return "pgsprite_probe_" + hex.EncodeToString(b[:]), nil
}
