package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/block/pg-sprite/pkg/statement"
)

// The catalog reads the bound proofs are rendered from: the parent's
// partition key and the statement's bound values resolved against it. Every
// read runs under catalogReadPath, so format_type and pg_get_partkeydef name
// the schema of every type and function outside pg_catalog, and every
// relation and operator here is pg_catalog-qualified (CO-9).

// readAttachKey reads the partition key of the partitioned table with the
// given OID: its strategy, and for each key column the key as SQL, its type,
// and its operator class's comparison operators. A key column with an
// explicit COLLATE or a non-default operator class, or an expression whose
// operator class takes a pseudo-type, is refused.
func readAttachKey(ctx context.Context, q querier, parentOID uint32) (attachKey, error) {
	var partstrat, keydef, parentLiteral string
	err := q.QueryRow(ctx, attachKeyParentSQL, parentOID).Scan(&partstrat, &keydef, &parentLiteral)
	if errors.Is(err, pgx.ErrNoRows) {
		return attachKey{}, fmt.Errorf("%w: table %d is not a partitioned table", ErrNotPartitionedTable, parentOID)
	}
	if err != nil {
		return attachKey{}, fmt.Errorf("read the partition key of table %d: %w", parentOID, err)
	}
	strategy, elems, err := statement.PartitionKey(keydef)
	if err != nil {
		return attachKey{}, err
	}
	if want := catalogStrategy(partstrat); strategy != want {
		return attachKey{}, fmt.Errorf("%w: the partition key %q of table %d parsed as %s, and the catalog records %s",
			ErrInvariantViolation, keydef, parentOID, strategy, want)
	}
	columns, err := readAttachKeyColumns(ctx, q, parentOID, strategy, elems)
	if err != nil {
		return attachKey{}, err
	}
	return attachKey{strategy: strategy, parentLiteral: parentLiteral, columns: columns}, nil
}

const attachKeyParentSQL = `SELECT pt.partstrat::pg_catalog.text,
		       pg_catalog.pg_get_partkeydef(pt.partrelid),
		       pg_catalog.quote_literal(pg_catalog.format('%I.%I', n.nspname, c.relname))
		  FROM pg_catalog.pg_partitioned_table pt
		  JOIN pg_catalog.pg_class c ON c.oid OPERATOR(pg_catalog.=) pt.partrelid
		  JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
		 WHERE pt.partrelid OPERATOR(pg_catalog.=) $1`

func catalogStrategy(partstrat string) statement.PartitionStrategy {
	switch partstrat {
	case "r":
		return statement.PartitionByRange
	case "l":
		return statement.PartitionByList
	case "h":
		return statement.PartitionByHash
	default:
		return statement.PartitionStrategy(partstrat)
	}
}

// attachKeyColumnsSQL lists the key columns in key order: the column's name
// (empty for an expression), the type its bound values take, as format_type
// prints it (the column's type and typmod, or the operator class's input
// type for an expression), whether that type is a pseudo-type or
// collatable, and the operator class's family and input type.
const attachKeyColumnsSQL = `SELECT k.attnum::pg_catalog.int4,
		       COALESCE(a.attname::pg_catalog.text, ''),
		       pg_catalog.format_type(t.oid, a.atttypmod),
		       t.typtype OPERATOR(pg_catalog.=) 'p',
		       t.typcollation OPERATOR(pg_catalog.<>) 0,
		       opc.opcfamily,
		       opc.opcintype
		  FROM pg_catalog.pg_partitioned_table pt
		 CROSS JOIN LATERAL ROWS FROM (
		         pg_catalog.unnest(pt.partattrs::pg_catalog.int2[]),
		         pg_catalog.unnest(pt.partclass::pg_catalog.oid[])) WITH ORDINALITY AS k(attnum, opclass, n)
		  JOIN pg_catalog.pg_opclass opc ON opc.oid OPERATOR(pg_catalog.=) k.opclass
		  LEFT JOIN pg_catalog.pg_attribute a ON a.attrelid OPERATOR(pg_catalog.=) pt.partrelid
		        AND a.attnum OPERATOR(pg_catalog.=) k.attnum
		  JOIN pg_catalog.pg_type t ON t.oid OPERATOR(pg_catalog.=) COALESCE(a.atttypid, opc.opcintype)
		 WHERE pt.partrelid OPERATOR(pg_catalog.=) $1
		 ORDER BY k.n`

type attachKeyColumnRow struct {
	attnum     int32
	name       string
	typeSQL    string
	pseudo     bool
	collatable bool
	opfamily   uint32
	opcintype  uint32
}

func readAttachKeyColumns(ctx context.Context, q querier, parentOID uint32, strategy statement.PartitionStrategy, elems []statement.PartitionKeyElem) ([]attachKeyColumn, error) {
	rows, err := q.Query(ctx, attachKeyColumnsSQL, parentOID)
	if err != nil {
		return nil, fmt.Errorf("read the partition key columns of table %d: %w", parentOID, err)
	}
	collected, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (attachKeyColumnRow, error) {
		var c attachKeyColumnRow
		err := r.Scan(&c.attnum, &c.name, &c.typeSQL, &c.pseudo, &c.collatable, &c.opfamily, &c.opcintype)
		return c, err
	})
	if err != nil {
		return nil, fmt.Errorf("read the partition key columns of table %d: %w", parentOID, err)
	}
	if len(collected) != len(elems) {
		return nil, fmt.Errorf("%w: table %d has %d partition key columns in the catalog and %d in its printed key",
			ErrInvariantViolation, parentOID, len(collected), len(elems))
	}
	columns := make([]attachKeyColumn, 0, len(collected))
	for i, row := range collected {
		col, err := attachKeyColumnFrom(row, elems[i])
		if err != nil {
			return nil, fmt.Errorf("partition key column %d of table %d: %w", i+1, parentOID, err)
		}
		if strategy != statement.PartitionByHash {
			if err := readKeyOperators(ctx, q, row, &col); err != nil {
				return nil, fmt.Errorf("partition key column %d of table %d: %w", i+1, parentOID, err)
			}
		}
		columns = append(columns, col)
	}
	return columns, nil
}

// attachKeyColumnFrom pairs a catalog key column with its printed element.
func attachKeyColumnFrom(row attachKeyColumnRow, elem statement.PartitionKeyElem) (attachKeyColumn, error) {
	if elem.Collated || elem.OpClass {
		return attachKeyColumn{}, fmt.Errorf("%w: the key has an explicit COLLATE or a non-default operator class", errAttachBoundUnprovable)
	}
	col := attachKeyColumn{typeSQL: row.typeSQL, collatable: row.collatable}
	switch {
	case row.attnum > 0 && elem.Column == row.name:
		col.expr = pgx.Identifier{row.name}.Sanitize()
	case row.attnum == 0 && elem.Expression != "":
		if row.pseudo {
			return attachKeyColumn{}, fmt.Errorf("%w: the key expression %s has an operator class over a pseudo-type, so its values' type is not known",
				errAttachBoundUnprovable, elem.Expression)
		}
		col.expr = "(" + elem.Expression + ")"
	default:
		return attachKeyColumn{}, fmt.Errorf("%w: the catalog records attnum %d (%q) and the printed key %+v",
			ErrInvariantViolation, row.attnum, row.name, elem)
	}
	return col, nil
}

// keyOperatorsSQL lists the btree comparison operators of an operator
// family for one input type, by strategy number (1 <, 2 <=, 3 =, 4 >=, 5 >).
// These are the operators the server's partition constraint uses.
const keyOperatorsSQL = `SELECT ao.amopstrategy::pg_catalog.int4, n.nspname::pg_catalog.text, o.oprname::pg_catalog.text
		  FROM pg_catalog.pg_amop ao
		  JOIN pg_catalog.pg_operator o ON o.oid OPERATOR(pg_catalog.=) ao.amopopr
		  JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) o.oprnamespace
		 WHERE ao.amopfamily OPERATOR(pg_catalog.=) $1
		   AND ao.amoplefttype OPERATOR(pg_catalog.=) $2
		   AND ao.amoprighttype OPERATOR(pg_catalog.=) $2
		   AND ao.amoppurpose OPERATOR(pg_catalog.=) 's'`

func readKeyOperators(ctx context.Context, q querier, row attachKeyColumnRow, col *attachKeyColumn) error {
	rows, err := q.Query(ctx, keyOperatorsSQL, row.opfamily, row.opcintype)
	if err != nil {
		return fmt.Errorf("read the key's comparison operators: %w", err)
	}
	type op struct {
		strategy     int32
		schema, name string
	}
	ops, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (op, error) {
		var o op
		err := r.Scan(&o.strategy, &o.schema, &o.name)
		return o, err
	})
	if err != nil {
		return fmt.Errorf("read the key's comparison operators: %w", err)
	}
	slots := map[int32]*string{1: &col.lt, 2: &col.le, 3: &col.eq, 4: &col.ge, 5: &col.gt}
	for _, o := range ops {
		slot, ok := slots[o.strategy]
		if !ok {
			continue
		}
		rendered, err := operatorSQL(o.schema, o.name)
		if err != nil {
			return err
		}
		*slot = rendered
	}
	for strategy, slot := range slots {
		if *slot == "" {
			return fmt.Errorf("%w: the key's operator family %d has no strategy %d operator for its type", errAttachBoundUnprovable, row.opfamily, strategy)
		}
	}
	return nil
}

// operatorSQL spells an operator schema-qualified: OPERATOR(schema.op). An
// operator's name is a run of operator characters, not an identifier, so it
// cannot be quoted; one with any other character is refused.
func operatorSQL(schema, name string) (string, error) {
	const operatorChars = "+-*/<>=~!@#%^&|`?"
	if name == "" || strings.Trim(name, operatorChars) != "" {
		return "", fmt.Errorf("%w: operator name %q has a character outside the operator set", ErrInvariantViolation, name)
	}
	return "OPERATOR(" + pgx.Identifier{schema}.Sanitize() + "." + name + ")", nil
}

// resolveAttachBound resolves the statement's bound against the key: every
// constant becomes a literal the server quoted, cast to its key column's
// type, and for a range bound the equal prefix is decided by the key's own
// equality operator. A value the key's type does not accept fails here, as
// the attach would.
func resolveAttachBound(ctx context.Context, q querier, key attachKey, b statement.PartitionBound) (attachBound, error) {
	if b.Default {
		return attachBound{}, fmt.Errorf("%w: attaching a DEFAULT partition needs a proof against every other partition's bound", errAttachBoundUnprovable)
	}
	if b.Strategy != key.strategy {
		return attachBound{}, fmt.Errorf("%w: the bound is a %s bound and the table is partitioned by %s",
			errAttachBoundUnprovable, b.Strategy, key.strategy)
	}
	out := attachBound{strategy: b.Strategy, modulus: b.Modulus, remainder: b.Remainder}
	var err error
	switch b.Strategy {
	case statement.PartitionByRange:
		if !rangeBoundFitsKey(b, len(key.columns)) {
			return attachBound{}, fmt.Errorf("%w: the range bound has %d and %d values for a %d-column key",
				errAttachBoundUnprovable, len(b.Lower), len(b.Upper), len(key.columns))
		}
		if out.lower, err = resolveValues(ctx, q, key.columns, b.Lower); err != nil {
			return attachBound{}, err
		}
		if out.upper, err = resolveValues(ctx, q, key.columns, b.Upper); err != nil {
			return attachBound{}, err
		}
		if out.equalPrefix, err = equalRangePrefix(ctx, q, key.columns, out.lower, out.upper); err != nil {
			return attachBound{}, err
		}
	case statement.PartitionByList:
		columns := make([]attachKeyColumn, len(b.List))
		for i := range columns {
			columns[i] = key.columns[0]
		}
		if out.list, err = resolveValues(ctx, q, columns, b.List); err != nil {
			return attachBound{}, err
		}
	case statement.PartitionByHash:
		// A hash bound's modulus and remainder are integers the grammar
		// read; there is no value to resolve.
	default:
		return attachBound{}, fmt.Errorf("%w: unknown partition strategy %q", ErrInvariantViolation, b.Strategy)
	}
	return out, nil
}

// rangeBoundFitsKey reports whether both sides of a range bound have one
// value per key column.
func rangeBoundFitsKey(b statement.PartitionBound, columns int) bool {
	return len(b.Lower) == columns && len(b.Upper) == columns
}

const quoteLiteralsSQL = `SELECT pg_catalog.quote_literal(v)
		  FROM ROWS FROM (pg_catalog.unnest($1::pg_catalog.text[])) WITH ORDINALITY AS t(v, n)
		 ORDER BY n`

// resolveValues renders each constant as CAST(<quoted> AS <type>) for the
// key column at the same position, with the server quoting the text.
func resolveValues(ctx context.Context, q querier, columns []attachKeyColumn, datums []statement.BoundDatum) ([]boundValue, error) {
	var texts []string
	for _, d := range datums {
		if d.Kind == statement.BoundValue {
			texts = append(texts, d.Value)
		}
	}
	quoted := make([]string, 0, len(texts))
	if len(texts) > 0 {
		rows, err := q.Query(ctx, quoteLiteralsSQL, texts)
		if err != nil {
			return nil, fmt.Errorf("quote the bound's values: %w", err)
		}
		if quoted, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			return nil, fmt.Errorf("quote the bound's values: %w", err)
		}
		if len(quoted) != len(texts) {
			return nil, fmt.Errorf("%w: quoted %d of the bound's %d values", ErrInvariantViolation, len(quoted), len(texts))
		}
	}
	values := make([]boundValue, len(datums))
	next := 0
	for i, d := range datums {
		values[i] = boundValue{kind: d.Kind}
		if d.Kind == statement.BoundValue {
			values[i].literal = "CAST(" + quoted[next] + " AS " + columns[i].typeSQL + ")"
			next++
		}
	}
	return values, nil
}

// equalRangePrefix counts the leading key columns whose lower and upper
// values are both constants and equal under the column's equality operator,
// as get_qual_for_range decides it.
func equalRangePrefix(ctx context.Context, q querier, columns []attachKeyColumn, lower, upper []boundValue) (int, error) {
	for i, c := range columns {
		if lower[i].kind != statement.BoundValue || upper[i].kind != statement.BoundValue {
			return i, nil
		}
		var equal bool
		if err := q.QueryRow(ctx, "SELECT "+lower[i].literal+" "+c.eq+" "+upper[i].literal).Scan(&equal); err != nil {
			return 0, fmt.Errorf("compare the range bound's values on key column %d: %w", i+1, err)
		}
		if !equal {
			return i, nil
		}
	}
	return len(columns), nil
}

// deriveAttachBoundCheck reads the parent's partition key and renders the
// bound's proofs (attachBoundCheck) in one catalog read.
func deriveAttachBoundCheck(ctx context.Context, tx briefTx, parentOID uint32, bound statement.PartitionBound) (check, defaultExclusion string, err error) {
	err = tx.read(ctx, "derive the bound proofs of the attach", func(ctx context.Context, q querier) error {
		key, err := readAttachKey(ctx, q, parentOID)
		if err != nil {
			return err
		}
		resolved, err := resolveAttachBound(ctx, q, key, bound)
		if err != nil {
			return err
		}
		check, defaultExclusion, err = attachBoundCheck(key, resolved)
		return err
	})
	return check, defaultExclusion, err
}
