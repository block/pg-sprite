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
// read any table, the live one included. A column added in place whose
// type or default resolves to anything a user defined is reported as
// statement.AddColumnInPlaceRunsUserCode, so that evaluation is never
// taken as catalog-only. A probe never waits for a lock:
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
// cause. A column added in place that is NOT NULL with no value for the
// existing rows is reported as statement.AddColumnInPlaceScans, since the
// server reads the whole table to check it, and one whose type or default
// evaluates code a user defined as statement.AddColumnInPlaceRunsUserCode,
// since that code can read any table. A probe that cannot take a
// lock at once leaves the column unproven.
func proveAddColumn(ctx context.Context, tx pgx.Tx, probeTable, column, probe string) (statement.AddColumnProof, error) {
	added, err := probeRewrite(ctx, tx, probeTable, column, func(string) string { return probe })
	switch {
	case errors.Is(err, errProbeLockNotAvailable):
		return statement.AddColumnUnproven, nil
	case err != nil:
		return "", err
	case !added.rewrites && added.checksNotNull:
		return statement.AddColumnInPlaceScans, nil
	case !added.rewrites && added.runsUserCode:
		return statement.AddColumnInPlaceRunsUserCode, nil
	case !added.rewrites:
		return statement.AddColumnInPlace, nil
	}
	// typeName is format_type's rendering of the type the server resolved
	// for the column, quoted and qualified as needed for the probe's
	// search_path, so it names the same type here. A serial or identity
	// column reads back as its integer type, without the default its
	// sequence supplies.
	typeAlone, err := probeRewrite(ctx, tx, probeTable, column, func(probeIdent string) string {
		return "ALTER TABLE " + probeIdent + " ADD COLUMN " + pgx.Identifier{column}.Sanitize() + " " + added.typeName
	})
	switch {
	case errors.Is(err, errProbeLockNotAvailable):
		return statement.AddColumnUnproven, nil
	case err != nil:
		return "", fmt.Errorf("add the type of column %q alone: %w", column, err)
	case typeAlone.rewrites:
		return statement.AddColumnTypeRewrites, nil
	default:
		return statement.AddColumnDefaultRewrites, nil
	}
}

// probeOutcome is what one probe read back after adding a column to the
// probe table.
type probeOutcome struct {
	// rewrites is true when adding the column replaced the table's storage.
	rewrites bool
	// checksNotNull is true when the column is NOT NULL and the server
	// stored no value for the rows a table already has: on a table with
	// storage it then reads every row to prove none exists, whether or not
	// it rewrites.
	checksNotNull bool
	// typeName is the type the server resolved for the column.
	typeName string
	// runsUserCode is true when the column's type, or anything its stored
	// default refers to, is defined outside PostgreSQL's built-in catalog
	// (see runsUserCodeQuery).
	runsUserCode bool
}

// firstNormalObjectIDLiteral is PostgreSQL's FirstNormalObjectId: every object
// initdb creates, the built-in catalog, has a lower OID, and every object
// created afterwards has this one or higher.
const firstNormalObjectIDLiteral = "16384"

// runsUserCodeQuery reports whether adding column $2 to the probe table $1
// evaluated something defined outside the built-in catalog. That is the
// column's type when it is a user-defined type other than an enum (or an
// array of one), since a domain brings its own default and checks and a
// composite or range can carry a domain; and anything the stored default
// depends on that a user defined, such as a function, operator, cast, or
// such a type. Built-in functions, operators, and types run fixed code
// that reads no user table, and an enum's input reads only the catalog.
// A collation runs no SQL, and the only relation a constant default can
// depend on is one a regclass literal names, which is resolved from the
// catalog when the statement is parsed.
//
// INV: CO-9 — pg_catalog-qualified like the probe's other reads.
const runsUserCodeQuery = `WITH col AS (
	SELECT a.attrelid, a.attnum, a.atttypid FROM pg_catalog.pg_attribute a
	WHERE a.attrelid OPERATOR(pg_catalog.=) $1::pg_catalog.regclass
	  AND a.attname OPERATOR(pg_catalog.=) $2
), refs AS (
	SELECT 'pg_catalog.pg_type'::pg_catalog.regclass AS refclassid, col.atttypid AS refobjid FROM col
	UNION ALL
	SELECT dep.refclassid, dep.refobjid FROM col
	JOIN pg_catalog.pg_attrdef d ON d.adrelid OPERATOR(pg_catalog.=) col.attrelid
	  AND d.adnum OPERATOR(pg_catalog.=) col.attnum
	JOIN pg_catalog.pg_depend dep ON dep.classid OPERATOR(pg_catalog.=) 'pg_catalog.pg_attrdef'::pg_catalog.regclass
	  AND dep.objid OPERATOR(pg_catalog.=) d.oid
	WHERE dep.refclassid OPERATOR(pg_catalog.<>) 'pg_catalog.pg_class'::pg_catalog.regclass
	  AND dep.refclassid OPERATOR(pg_catalog.<>) 'pg_catalog.pg_collation'::pg_catalog.regclass
)
SELECT EXISTS (
	SELECT 1 FROM refs r
	WHERE r.refobjid OPERATOR(pg_catalog.>=) ` + firstNormalObjectIDLiteral + `::pg_catalog.oid
	  AND NOT EXISTS (
		SELECT 1 FROM pg_catalog.pg_type t
		LEFT JOIN pg_catalog.pg_type e ON t.typcategory OPERATOR(pg_catalog.=) 'A'
		  AND e.oid OPERATOR(pg_catalog.=) t.typelem
		WHERE r.refclassid OPERATOR(pg_catalog.=) 'pg_catalog.pg_type'::pg_catalog.regclass
		  AND t.oid OPERATOR(pg_catalog.=) r.refobjid
		  AND (t.typtype OPERATOR(pg_catalog.=) 'e' OR e.typtype OPERATOR(pg_catalog.=) 'e'
		       OR e.oid OPERATOR(pg_catalog.<) ` + firstNormalObjectIDLiteral + `::pg_catalog.oid)))`

// probeRewrite runs the ALTER that alter builds for the probe table's
// qualified name against a fresh probe table, inside a savepoint, and
// reports whether it replaced the table's storage, whether the server
// must read every row to check the column's NOT NULL, and the type it
// resolved for column. A lock the probe cannot take at once is
// errProbeLockNotAvailable. The savepoint is rolled back either way,
// dropping the probe table and releasing its locks.
func probeRewrite(ctx context.Context, tx pgx.Tx, probeTable, column string, alter func(probeIdent string) string) (outcome probeOutcome, err error) {
	probeIdent := pgx.Identifier{"pg_temp", probeTable}.Sanitize()
	if _, err := tx.Exec(ctx, "SAVEPOINT pgsprite_probe"); err != nil {
		return probeOutcome{}, fmt.Errorf("open the probe savepoint: %w", err)
	}
	if _, err := tx.Exec(ctx, "CREATE TEMPORARY TABLE "+pgx.Identifier{probeTable}.Sanitize()+" ()"); err != nil {
		return probeOutcome{}, probeTableError(err)
	}
	// INV: CO-9 — every function, operator, relation, and type in the
	// probe's reads is pg_catalog-qualified: the probe runs on the
	// statement's path, and a planted (oid, regclass) = there would
	// otherwise answer the attribute lookup.
	var before, after uint32
	if err := tx.QueryRow(ctx, "SELECT pg_catalog.pg_relation_filenode($1::pg_catalog.regclass)", probeIdent).Scan(&before); err != nil {
		return probeOutcome{}, fmt.Errorf("read the probe table's storage before adding column %q: %w", column, err)
	}
	_, err = tx.Exec(ctx, alter(probeIdent))
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &pgErr) && pgErr.Code == sqlstateLockNotAvailable:
		err = errProbeLockNotAvailable
	case err != nil:
		return probeOutcome{}, err
	default:
		// atthasmissing is set when the server stored one value for every
		// existing row (a fast default). A NOT NULL column without it gets
		// NULL in those rows, so the server scans the table to prove it
		// has none; that scan replaces no storage, so the filenode alone
		// cannot show it.
		if err := tx.QueryRow(ctx, `SELECT pg_catalog.pg_relation_filenode($1::pg_catalog.regclass),
			pg_catalog.format_type(atttypid, atttypmod), attnotnull AND NOT atthasmissing
			FROM pg_catalog.pg_attribute
			WHERE attrelid OPERATOR(pg_catalog.=) $1::pg_catalog.regclass
			  AND attname OPERATOR(pg_catalog.=) $2`, probeIdent, column).Scan(&after, &outcome.typeName, &outcome.checksNotNull); err != nil {
			return probeOutcome{}, fmt.Errorf("read the probe table's storage after adding column %q: %w", column, err)
		}
		outcome.rewrites = before != after
		if err := tx.QueryRow(ctx, runsUserCodeQuery, probeIdent, column).Scan(&outcome.runsUserCode); err != nil {
			return probeOutcome{}, fmt.Errorf("read what adding column %q to the probe table evaluates: %w", column, err)
		}
	}
	if _, rollbackErr := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT pgsprite_probe"); rollbackErr != nil {
		return probeOutcome{}, fmt.Errorf("roll back the probe savepoint: %w", rollbackErr)
	}
	return outcome, err
}

// probeTableError separates the server refusing the probe table, which
// only the deployment can fix, from any other failure to create it.
func probeTableError(err error) error {
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok {
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
	// INV: CO-9 — pg_catalog-qualified like the probe's own reads.
	rows, err := tx.Query(ctx, `SELECT a.attname FROM pg_catalog.pg_attribute a
		JOIN pg_catalog.pg_class c ON c.oid OPERATOR(pg_catalog.=) a.attrelid
		JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
		WHERE n.nspname OPERATOR(pg_catalog.=) $1 AND c.relname OPERATOR(pg_catalog.=) $2
		  AND a.attnum OPERATOR(pg_catalog.>) 0 AND NOT a.attisdropped
		  AND a.attname OPERATOR(pg_catalog.=) ANY($3)`, schema, table, names)
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
