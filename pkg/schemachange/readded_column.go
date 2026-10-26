package schemachange

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/block/pg-sprite/pkg/schemadiff"
)

// domainFill is what a column's domain type contributes to a row that
// leaves the column out: whether the domain, or any domain it is built on,
// refuses NULL, and whether the column's own type has a default the server
// takes when the column has none.
type domainFill struct {
	notNull    bool
	hasDefault bool
}

// readDomainFills reads, for every live column of the relation whose type
// is a domain, what that domain contributes when the column is left out.
// The server enforces NOT NULL from every domain down the base-type chain,
// but takes a default only from the column's own type, which holds the
// default its base domain had when it was created; so the NOT NULL walks
// the chain and the default is read from the first link only.
func readDomainFills(ctx context.Context, tx pgx.Tx, oid uint32) (map[string]domainFill, error) {
	rows, err := tx.Query(ctx, `
		WITH RECURSIVE chain AS (
			SELECT a.attname,
			       t.typbasetype,
			       t.typnotnull,
			       t.typdefaultbin IS NOT NULL AS has_default,
			       0 AS depth
			FROM pg_attribute a
			JOIN pg_type t ON t.oid = a.atttypid
			WHERE a.attrelid = $1
			  AND a.attnum > 0
			  AND NOT a.attisdropped
			  AND t.typtype = 'd'
			UNION ALL
			SELECT c.attname,
			       t.typbasetype,
			       t.typnotnull,
			       t.typdefaultbin IS NOT NULL,
			       c.depth + 1
			FROM chain c
			JOIN pg_type t ON t.oid = c.typbasetype
			WHERE t.typtype = 'd'
		)
		SELECT attname,
		       bool_or(typnotnull),
		       bool_or(has_default) FILTER (WHERE depth = 0)
		FROM chain
		GROUP BY attname`, oid)
	if err != nil {
		return nil, fmt.Errorf("read column domains: %w", err)
	}
	fills := make(map[string]domainFill)
	var name string
	var fill domainFill
	_, err = pgx.ForEachRow(rows, []any{&name, &fill.notNull, &fill.hasDefault}, func() error {
		fills[name] = fill
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read column domains: %w", err)
	}
	return fills, nil
}

// refuseUnfillableReadds refuses a gated statement that drops a column and
// adds one under the same name when the copy cannot fill the new column.
// A re-added column is a new column: the copy leaves it out and every row
// the copy writes takes the column's own default, or its domain's, as under
// a direct ALTER TABLE. That has nowhere to go for three columns, each
// refused here, before the build commits, instead of after the shadow
// exists:
//
//   - the primary key, which every chunk range, watermark, and streamed
//     change is keyed on, so a copy without it cannot run, whatever the new
//     column declares;
//   - the replacement of a source identity column, whose sequence the
//     handoff carries to the same column on the shadow and cannot carry to
//     a new one, whatever the new column declares;
//   - a column that refuses NULL, by its own NOT NULL or its domain's, with
//     nothing the server fills it from, which the first copied row would
//     violate.
//
// A re-added column the copy can leave to the server — nullable, with a
// default of its own or its domain's, a generated column, or an identity of
// its own replacing a plain column — is not refused here. Both position
// lists are in attribute order, as readColumnPositions returns them; pk is
// the source's primary key column, domains is the shadow's readDomainFills,
// and identities are the source's identity columns.
func refuseUnfillableReadds(schema, table, pk string, sourcePositions, shadowPositions []columnPosition, shadow schemadiff.Model, domains map[string]domainFill, identities []IdentityColumn) error {
	same := sameColumns(sourcePositions, shadowPositions)
	shadowColumns := make(map[string]schemadiff.Column, len(shadow.Columns))
	for _, c := range shadow.Columns {
		shadowColumns[c.Name] = c
	}
	sourceIdentity := make(map[string]bool, len(identities))
	for _, id := range identities {
		sourceIdentity[id.Column] = true
	}
	for _, source := range sourcePositions {
		column, readded := shadowColumns[source.name]
		if same[source.name] || !readded {
			continue
		}
		// INV: ST-11
		if source.name == pk {
			return refuse(CauseStatementReadd, nil, "the statement for %s.%s drops and re-adds the primary key %s, which the copy is keyed on; a default does not lift this, so change the key in place with ALTER COLUMN TYPE instead", schema, table, source.name)
		}
		if sourceIdentity[source.name] {
			return refuse(CauseStatementReadd, nil, "the statement for %s.%s drops and re-adds identity column %s, whose sequence cannot be handed to a new column; a default does not lift this, so drop the column in one schema change and add the new one in the next", schema, table, source.name)
		}
		if requiresAValueTheCopyCannotGive(column, domains[source.name]) {
			return refuse(CauseStatementReadd, nil, "the statement for %s.%s drops and re-adds column %s, which refuses NULL and has no default of its own or from its type, so the copy has no value to give it; give the re-added column a default", schema, table, source.name)
		}
	}
	return nil
}

// requiresAValueTheCopyCannotGive reports whether a new column the copy
// leaves out would reject every copied row: it refuses NULL, by its own
// NOT NULL or its domain's, and the server has nothing to fill it with — no
// default of its own, no identity, no domain default. A generated column's
// Default holds its generation expression, so a non-empty Default covers
// generated columns as well as column defaults.
func requiresAValueTheCopyCannotGive(c schemadiff.Column, domain domainFill) bool {
	if !c.NotNull && !domain.notNull {
		return false
	}
	if c.Default != "" {
		return false
	}
	if c.Identity != schemadiff.IdentityNone {
		return false
	}
	return !domain.hasDefault
}
