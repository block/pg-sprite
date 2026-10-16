package statement

import (
	"errors"
	"fmt"

	pganalyze "github.com/pganalyze/pg_query_go/v6"
)

// ErrRepeatedAddColumn means an ALTER TABLE adds the same column name in
// more than one subcommand. Each addition can rewrite the table or not on
// its own terms, so one proof cannot stand for both.
var ErrRepeatedAddColumn = errors.New("the statement adds the same column more than once")

// AddColumnProbe is the probe for one ADD COLUMN: SQL adds the same column
// to the probe table, and IfNotExists records that the subcommand is
// skipped when the column already exists.
type AddColumnProbe struct {
	SQL         string
	IfNotExists bool
}

// AddColumnProbes returns, for each ADD COLUMN of an ALTER TABLE, a
// single-command ALTER TABLE that adds the same column to schema.table,
// keyed by column name. A probe keeps everything that decides whether the
// server rewrites the table to add the column: its type and collation, its
// DEFAULT, NOT NULL, and identity. It drops the inline CHECK, UNIQUE,
// PRIMARY KEY, REFERENCES, and EXCLUDE constraints, which scan the table or
// build an index but never rewrite it, and which a probe table cannot
// always carry, and an identity's SEQUENCE NAME, so the probe's sequence is
// created beside the probe table rather than in the named schema. A
// generated column gets no probe: the planner routes it by its generation
// alone, and its expression may name a column the same change adds, which
// the probe table does not have. A statement other than ALTER TABLE
// returns ErrNotAlterTable, and one that adds a column name twice returns
// ErrRepeatedAddColumn; an ALTER TABLE that adds no column, or only
// generated ones, returns an empty map.
func AddColumnProbes(sql, schema, table string) (map[string]AddColumnProbe, error) {
	node, err := parseSingle(sql)
	if err != nil {
		return nil, err
	}
	alter := node.GetAlterTableStmt()
	if alter == nil || alter.GetRelation() == nil {
		return nil, ErrNotAlterTable
	}
	probes := make(map[string]AddColumnProbe)
	added := make(map[string]bool)
	for _, raw := range alter.GetCmds() {
		cmd := raw.GetAlterTableCmd()
		if cmd.GetSubtype() != pganalyze.AlterTableType_AT_AddColumn {
			continue
		}
		def := cmd.GetDef().GetColumnDef()
		if def == nil {
			return nil, fmt.Errorf("ADD COLUMN without a column definition in %q", sql)
		}
		if added[def.GetColname()] {
			return nil, fmt.Errorf("%w: column %q in %q", ErrRepeatedAddColumn, def.GetColname(), sql)
		}
		added[def.GetColname()] = true
		if generated(def) {
			continue
		}
		kept := make([]*pganalyze.Node, 0, len(def.GetConstraints()))
		for _, c := range def.GetConstraints() {
			if !decidesRewrite(c.GetConstraint().GetContype()) {
				continue
			}
			if c.GetConstraint().GetContype() == pganalyze.ConstrType_CONSTR_IDENTITY {
				withoutSequenceName(c.GetConstraint())
			}
			kept = append(kept, c)
		}
		def.Constraints = kept
		probe := &pganalyze.Node{Node: &pganalyze.Node_AlterTableStmt{AlterTableStmt: &pganalyze.AlterTableStmt{
			Relation: &pganalyze.RangeVar{Schemaname: schema, Relname: table, Inh: true, Relpersistence: "p"},
			Cmds:     []*pganalyze.Node{raw},
			Objtype:  alter.GetObjtype(),
		}}}
		out, err := deparseOne(probe)
		if err != nil {
			return nil, fmt.Errorf("deparse the probe for column %q: %w", def.GetColname(), err)
		}
		probes[def.GetColname()] = AddColumnProbe{SQL: out, IfNotExists: cmd.GetMissingOk()}
	}
	return probes, nil
}

// withoutSequenceName removes the SEQUENCE NAME option from an identity
// constraint, leaving the sequence parameters that the column's values
// depend on.
func withoutSequenceName(c *pganalyze.Constraint) {
	kept := make([]*pganalyze.Node, 0, len(c.GetOptions()))
	for _, opt := range c.GetOptions() {
		if opt.GetDefElem().GetDefname() == "sequence_name" {
			continue
		}
		kept = append(kept, opt)
	}
	c.Options = kept
}

// decidesRewrite reports whether an inline column constraint is one that
// can make adding the column rewrite the table.
func decidesRewrite(kind pganalyze.ConstrType) bool {
	switch kind {
	case pganalyze.ConstrType_CONSTR_DEFAULT,
		pganalyze.ConstrType_CONSTR_IDENTITY,
		pganalyze.ConstrType_CONSTR_NOTNULL,
		pganalyze.ConstrType_CONSTR_NULL:
		return true
	default:
		return false
	}
}

// generated reports whether a column definition is a generated column.
func generated(def *pganalyze.ColumnDef) bool {
	for _, c := range def.GetConstraints() {
		if c.GetConstraint().GetContype() == pganalyze.ConstrType_CONSTR_GENERATED {
			return true
		}
	}
	return false
}
