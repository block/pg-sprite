package schemachange

import (
	"context"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
)

// handOffSequences completes D5 on the live table, which by now bears the
// source's name: every sequence a source column owned through OWNED BY is
// re-owned to the live column, so it survives the old table's drop, and
// every identity column is recreated on the live table from the source
// sequence's options and set to the source sequence's exact position. It
// returns the identity columns as the handoff declared them, for confirmSwap
// to check the catalog against and for the SwappedTable to report.
func handOffSequences(ctx context.Context, tx pgx.Tx, ready CutoverReady) ([]IdentityColumn, error) {
	built := ready.built
	schema, live := built.Schema(), built.SourceTable()
	for _, s := range ready.sequences {
		if err := reownSequence(ctx, tx, s, schema, live); err != nil {
			return nil, err
		}
	}
	source := built.IdentityColumns()
	identities := make([]IdentityColumn, 0, len(source))
	for _, id := range source {
		declared, err := handOffIdentity(ctx, tx, schema, live, id)
		if err != nil {
			return nil, err
		}
		identities = append(identities, declared)
	}
	return identities, nil
}

// reownSequence moves a shared serial/nextval sequence's OWNED BY edge to
// the live table's column. The sequence keeps its name (D8).
func reownSequence(ctx context.Context, tx pgx.Tx, s OwnedSequence, schema, live string) error {
	sql := "ALTER SEQUENCE " + pgx.Identifier{s.SequenceSchema, s.SequenceName}.Sanitize() +
		" OWNED BY " + pgx.Identifier{schema, live, s.Column}.Sanitize()
	if _, err := tx.Exec(ctx, sql); err != nil {
		return fmt.Errorf("re-own sequence %s.%s to %s.%s.%s: %w", s.SequenceSchema, s.SequenceName, schema, live, s.Column, err)
	}
	return nil
}

// handOffIdentity turns the live table's plain DEFAULT nextval(<source
// sequence>) column back into the identity column the source had, and
// returns the identity as declared. The source's identity sequence already
// wears its _old name, so: read its position and type from the sequence
// itself, drop the handoff default, add identity with the source
// sequence's declared options — a bound the source took from its column's
// old type moved to the live column's type, as the server's own ALTER
// COLUMN … TYPE does when it retypes an identity column — rename the
// server-named new sequence to the name the source's held, carry the
// source sequence's grants onto it, and set it to the position read —
// last_value and is_called together, through bind parameters, so a
// never-advanced source leaves the new sequence at the same not-yet-issued
// value instead of skipping it.
func handOffIdentity(ctx context.Context, tx pgx.Tx, schema, live string, id IdentityColumn) (IdentityColumn, error) {
	oldSequence := sequenceRef{schema: id.SequenceSchema, name: OldDependentName(schema, live, id.SequenceName)}
	var lastValue int64
	var isCalled bool
	if err := tx.QueryRow(ctx, "SELECT last_value, is_called FROM "+oldSequence.sql()).Scan(&lastValue, &isCalled); err != nil {
		return IdentityColumn{}, fmt.Errorf("read position of identity sequence %s: %w", oldSequence.sql(), err)
	}
	oldType, newType, err := readIdentityTypes(ctx, tx, oldSequence, schema, live, id.Column)
	if err != nil {
		return IdentityColumn{}, err
	}
	declared := id
	declared.Options = rebaseSequenceBounds(id.Options, oldType, newType)
	table := pgx.Identifier{schema, live}.Sanitize()
	column := pgx.Identifier{id.Column}.Sanitize()
	if _, err := tx.Exec(ctx, "ALTER TABLE "+table+" ALTER COLUMN "+column+" DROP DEFAULT"); err != nil {
		return IdentityColumn{}, fmt.Errorf("drop handoff default on %s.%s.%s: %w", schema, live, id.Column, err)
	}
	if _, err := tx.Exec(ctx, "ALTER TABLE "+table+" ALTER COLUMN "+column+" ADD "+identityClause(declared)); err != nil {
		return IdentityColumn{}, fmt.Errorf("add identity on %s.%s.%s: %w", schema, live, id.Column, err)
	}
	created, err := identitySequenceOf(ctx, tx, schema, live, id.Column)
	if err != nil {
		return IdentityColumn{}, err
	}
	if created.SequenceName != id.SequenceName {
		if err := renameSequence(ctx, tx, created.SequenceSchema, created.SequenceName, id.SequenceName); err != nil {
			return IdentityColumn{}, err
		}
	}
	sequence := sequenceRef{schema: id.SequenceSchema, name: id.SequenceName}
	if err := carrySequenceGrants(ctx, tx, oldSequence, sequence); err != nil {
		return IdentityColumn{}, err
	}
	if _, err := tx.Exec(ctx, "SELECT setval($1::regclass, $2, $3)", sequence.sql(), lastValue, isCalled); err != nil {
		return IdentityColumn{}, fmt.Errorf("set identity sequence %s to (%d, %t): %w", sequence.sql(), lastValue, isCalled, err)
	}
	return declared, nil
}

// readIdentityTypes reads the type the source's identity sequence was
// declared with and the type the live column now has. They differ exactly
// when the gated statement retyped the column.
func readIdentityTypes(ctx context.Context, tx pgx.Tx, oldSequence sequenceRef, schema, live, column string) (oldType, newType uint32, err error) {
	err = tx.QueryRow(ctx, `
		SELECT q.seqtypid, a.atttypid
		FROM pg_sequence q, pg_attribute a
		WHERE q.seqrelid = $1::regclass
		  AND a.attrelid = $2::regclass AND a.attname = $3 AND NOT a.attisdropped`,
		oldSequence.sql(), pgx.Identifier{schema, live}.Sanitize(), column).Scan(&oldType, &newType)
	if err != nil {
		return 0, 0, fmt.Errorf("read types of identity %s.%s.%s and its source sequence %s: %w", schema, live, column, oldSequence.sql(), err)
	}
	return oldType, newType, nil
}

// identityClause renders GENERATED … AS IDENTITY with the source sequence's
// declared options. The values are the catalog's own int64s rendered
// through strconv, never user text; the counter's position is not among
// them and is set afterwards from the sequence itself.
func identityClause(id IdentityColumn) string {
	generated := "GENERATED BY DEFAULT"
	if id.Always {
		generated = "GENERATED ALWAYS"
	}
	cycle := "NO CYCLE"
	if id.Options.Cycle {
		cycle = "CYCLE"
	}
	o := id.Options
	return generated + " AS IDENTITY (" +
		"INCREMENT BY " + strconv.FormatInt(o.Increment, 10) +
		" MINVALUE " + strconv.FormatInt(o.Min, 10) +
		" MAXVALUE " + strconv.FormatInt(o.Max, 10) +
		" START WITH " + strconv.FormatInt(o.Start, 10) +
		" CACHE " + strconv.FormatInt(o.Cache, 10) +
		" " + cycle + ")"
}

// identitySequenceOf finds the sequence the server created for the live
// table's identity column, by the internal pg_depend edge the catalog
// records, so the rename that follows never guesses at a server-chosen name.
func identitySequenceOf(ctx context.Context, tx pgx.Tx, schema, table, column string) (IdentityColumn, error) {
	oid, err := resolveRelation(ctx, tx, schema, table)
	if err != nil {
		return IdentityColumn{}, err
	}
	identities, err := readIdentityColumns(ctx, tx, oid)
	if err != nil {
		return IdentityColumn{}, fmt.Errorf("live %s.%s: %w", schema, table, err)
	}
	for _, id := range identities {
		if id.Column == column {
			return id, nil
		}
	}
	// INV: ST-5
	return IdentityColumn{}, refuse(CauseSwapMismatch, nil, "live %s.%s has no identity column %s after the handoff", schema, table, column)
}
