package schemadiff

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/statement"
)

// ErrDesiredPartitionOf is returned for a desired file whose CREATE TABLE
// is PARTITION OF a parent. The scratch build would attach the scratch
// table to the live partitioned parent, which takes ACCESS EXCLUSIVE on the
// parent and blocks its reads and writes until the scratch transaction
// rolls back, so the file is refused before anything runs.
var ErrDesiredPartitionOf = errors.New("a desired CREATE TABLE ... PARTITION OF cannot be planned: building it would lock the live partitioned parent")

// ErrDesiredInherits is returned for a desired file whose CREATE TABLE
// INHERITS from a parent. Classic inheritance is not planned, and the
// scratch build would bind the parent to the live table of that name, lock
// it, and add an inheritance edge to it until the scratch transaction rolls
// back, so the file is refused before anything runs.
var ErrDesiredInherits = errors.New("a desired CREATE TABLE ... INHERITS cannot be planned: building it would lock the live parent and add an inheritance edge to it")

// IntrospectDesired materializes a desired-state schema on a scratch schema
// and introspects it into the canonical model: execute-and-introspect, the
// decided way the engine understands DDL semantics. The scratch schema is
// created inside a single transaction that is always rolled back — nothing
// the desired file defines ever persists, no CREATEDB privilege is needed,
// and server-version and extension parity with the live table hold by
// construction because it runs on the same database.
//
// schema is the target schema the desired table lives in. A table the
// desired foreign keys reference is read from it: the scratch schema holds an
// empty copy of each one, so no foreign key ever binds to a live table, and
// planning takes only the ACCESS SHARE lock a plain SELECT takes on it (see
// createReferencedStandIns).
func IntrospectDesired(ctx context.Context, db *pgxpool.Pool, schema string, desired statement.DesiredSchema) (Model, error) {
	return introspectDesiredStatements(ctx, db, desired.Table(), desired.Statements(), referencedFrom{schema: schema, tables: desired.ReferencedTables()})
}

// referencedFrom names the live tables the scratch schema copies before the
// desired statements run, and the schema they are read from. The zero value
// copies nothing.
type referencedFrom struct {
	schema string
	tables []string
}

func introspectDesiredStatements(ctx context.Context, db *pgxpool.Pool, table string, statements []statement.Statement, refs referencedFrom) (Model, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return Model{}, fmt.Errorf("begin scratch transaction: %w", err)
	}
	return introspectDesiredTransaction(ctx, tx, table, statements, refs)
}

// introspectDesiredTransaction owns tx, which may be a savepoint. Every exit
// rolls it back, leaving its parent's live target and settings untouched.
func introspectDesiredTransaction(ctx context.Context, tx pgx.Tx, table string, statements []statement.Statement, refs referencedFrom) (Model, error) {
	// The scratch transaction is never committed: rollback is the cleanup
	// path for success and failure alike, so the redundant-closer exception
	// does not apply — this rollback is load-bearing and its error is
	// surfaced on the success path below.
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()

	if err := refuseLiveParent(statements); err != nil {
		return Model{}, err
	}

	scratch, err := scratchSchemaName()
	if err != nil {
		return Model{}, err
	}

	if _, err := tx.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{scratch}.Sanitize()); err != nil {
		return Model{}, fmt.Errorf("create scratch schema: %w", err)
	}
	// Unqualified desired statements must land on the scratch schema, while
	// the target schema's own types and functions, and extension types
	// installed in public, stay resolvable by the names the live table's
	// definitions print with. The path is built by dbconn so it upholds
	// CO-9 like the session's.
	path := scratchSearchPath(scratch, refs.schema)
	if _, err := tx.Exec(ctx, dbconn.LocalSearchPath(path...)); err != nil {
		return Model{}, fmt.Errorf("set scratch search_path: %w", err)
	}
	if err := createReferencedStandIns(ctx, tx, scratch, refs.schema, refs.tables); err != nil {
		return Model{}, err
	}
	// Statements arrive in execution order — the CREATE TABLE first — so
	// an index never replays before the table it targets exists.
	policyPath := false
	for _, st := range statements {
		if st.Kind() == statement.KindProvisioning && !policyPath {
			// Helpers and types in policy expressions must be qualified. Do not
			// accidentally bind an unqualified helper in public instead of the
			// target schema. The target itself remains scratch-relative.
			if _, err := tx.Exec(ctx, dbconn.LocalSearchPath(scratch)); err != nil {
				return Model{}, fmt.Errorf("set policy scratch search_path: %w", err)
			}
			policyPath = true
		}
		if _, err := tx.Exec(ctx, st.SQL()); err != nil {
			return Model{}, fmt.Errorf("execute desired statement on scratch schema: %w", err)
		}
	}
	if err := confirmForeignKeysBindScratch(ctx, tx, scratch, table); err != nil {
		return Model{}, err
	}
	m, err := introspectOnPath(ctx, tx, scratch, table, path)
	if err != nil {
		return Model{}, fmt.Errorf("introspect desired state: %w", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		return Model{}, fmt.Errorf("roll back scratch schema: %w", err)
	}
	return m, nil
}

// refuseLiveParent refuses a desired CREATE TABLE that names a parent,
// PARTITION OF or INHERITS, before the scratch build runs it: the parent
// would resolve to a live table (see ErrDesiredPartitionOf and
// ErrDesiredInherits).
func refuseLiveParent(statements []statement.Statement) error {
	for _, st := range statements {
		if st.Kind() != statement.KindCreateTable {
			continue
		}
		ops, err := statement.ParseOps(st.SQL())
		if err != nil {
			return fmt.Errorf("parse desired CREATE TABLE: %w", err)
		}
		for _, op := range ops {
			if op.PartitionOf {
				return fmt.Errorf("desired table %q: %w", st.Table(), ErrDesiredPartitionOf)
			}
			if op.Inherits {
				return fmt.Errorf("desired table %q: %w", st.Table(), ErrDesiredInherits)
			}
		}
	}
	return nil
}

// scratchSearchPath is the search_path a scratch build runs and is
// introspected under: the scratch schema first, so every unqualified object
// the desired file creates lands there, then the path the live table is
// introspected under. A type or function the target schema defines then
// resolves from the file by its bare name, as it does for the live table,
// and both sides print it unqualified, so the two models compare equal. An
// empty target leaves only public behind the scratch schema.
func scratchSearchPath(scratch, target string) []string {
	if target == "" {
		return []string{scratch, "public"}
	}
	return append([]string{scratch}, searchPathFor(target)...)
}

// scratchSchemaName returns a collision-resistant scratch schema name. The
// name only has to be unique among concurrent scratch transactions on the
// same database; the schema itself never outlives its transaction. The
// prefix avoids "pg_", which PostgreSQL reserves for system schemas.
func scratchSchemaName() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate scratch schema name: %w", err)
	}
	return "pgsprite_scratch_" + hex.EncodeToString(b[:]), nil
}
