package schemadiff

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/statement"
)

// IntrospectDesired materializes a desired-state schema on a scratch schema
// and introspects it into the canonical model: execute-and-introspect, the
// decided way the engine understands DDL semantics. The scratch schema is
// created inside a single transaction that is always rolled back — nothing
// the desired file defines ever persists, no CREATEDB privilege is needed,
// and server-version and extension parity with the live table hold by
// construction because it runs on the same database.
func IntrospectDesired(ctx context.Context, db *pgxpool.Pool, desired statement.DesiredSchema) (Model, error) {
	scratch, err := scratchSchemaName()
	if err != nil {
		return Model{}, err
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return Model{}, fmt.Errorf("begin scratch transaction: %w", err)
	}
	// The scratch transaction is never committed: rollback is the cleanup
	// path for success and failure alike, so the redundant-closer exception
	// does not apply — this rollback is load-bearing and its error is
	// surfaced on the success path below.
	defer func() {
		_ = tx.Rollback(context.WithoutCancel(ctx))
	}()

	if _, err := tx.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{scratch}.Sanitize()); err != nil {
		return Model{}, fmt.Errorf("create scratch schema: %w", err)
	}
	// Unqualified desired statements must land on the scratch schema, while
	// extension types installed in public stay resolvable. search_path
	// cannot use bind parameters; the identifier is sanitized.
	setPath := "SET LOCAL search_path = " + pgx.Identifier{scratch}.Sanitize() + ", public"
	if _, err := tx.Exec(ctx, setPath); err != nil {
		return Model{}, fmt.Errorf("set scratch search_path: %w", err)
	}
	for _, st := range desired.Statements() {
		if _, err := tx.Exec(ctx, st.SQL()); err != nil {
			return Model{}, fmt.Errorf("execute desired statement on scratch schema: %w", err)
		}
	}
	m, err := introspectInTx(ctx, tx, scratch, desired.Table())
	if err != nil {
		return Model{}, fmt.Errorf("introspect desired state: %w", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		return Model{}, fmt.Errorf("roll back scratch schema: %w", err)
	}
	return m, nil
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
