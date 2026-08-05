package statement

import (
	"fmt"
	"strings"

	pganalyze "github.com/pganalyze/pg_query_go/v6"
	pgquery "github.com/wasilibs/go-pgquery"
)

// OpKind names one operation shape the classifier distinguishes. A single
// ALTER TABLE statement yields one Op per subcommand; index, rename, and
// schema statements yield exactly one.
type OpKind int

// The operation shapes ParseOps reports. OpUnrecognized is everything the engine
// does not recognize; the classifier refuses it.
const (
	OpUnrecognized OpKind = iota
	OpCreateTable
	OpAddColumn
	OpDropColumn
	OpAlterColumnType
	OpSetDefault
	OpDropDefault
	OpSetNotNull
	OpDropNotNull
	OpSetColumnOptions
	OpRenameColumn
	OpRenameTable
	OpRenameIndex
	OpSetSchema
	OpSetTablespace
	OpSetRelOptions
	OpAddConstraint
	OpValidateConstraint
	OpDropConstraint
	OpAttachPartition
	OpDetachPartition
	OpCreateIndex
	OpDropIndex
	OpReindex
)

// DefaultKind classifies the DEFAULT expression shape of an added column.
// Only a provable constant qualifies for PostgreSQL's fast default; any
// other expression is treated as volatile, conservatively.
type DefaultKind int

// The default shapes an added column can carry.
const (
	// DefaultNone: no DEFAULT clause.
	DefaultNone DefaultKind = iota
	// DefaultConstant: a literal (possibly type-cast) — fast-default safe.
	DefaultConstant
	// DefaultExpression: anything else — function calls, identity, serial.
	// The engine does not evaluate volatility offline; it assumes the worst.
	DefaultExpression
)

// ConstraintKind names the constraint families the classifier routes
// differently.
type ConstraintKind int

// The constraint families ParseOps distinguishes. ConstraintUnrecognized
// (e.g. EXCLUDE) has no known safe pattern and is refused.
const (
	ConstraintUnrecognized ConstraintKind = iota
	ConstraintPrimaryKey
	ConstraintUnique
	ConstraintCheck
	ConstraintForeignKey
	ConstraintNotNull
)

// Op is one parsed operation: the shape facts the classifier needs, nothing
// executable. Fields beyond Kind are populated only where meaningful for
// that kind; see each field's comment.
type Op struct {
	// Kind is the operation shape.
	Kind OpKind
	// Column is the target column for column operations.
	Column string
	// Name is the constraint or index name where the operation has one,
	// or the new name for renames.
	Name string
	// Columns are the plain key columns of an ADD PRIMARY KEY / UNIQUE;
	// empty when the keys are expressions.
	Columns []string
	// Constraint is the constraint family for OpAddConstraint.
	Constraint ConstraintKind
	// NotValid is true for ADD CONSTRAINT ... NOT VALID.
	NotValid bool
	// UsingIndex is true for ADD CONSTRAINT ... USING INDEX.
	UsingIndex bool
	// Concurrent is true when the statement carries CONCURRENTLY.
	Concurrent bool
	// Unique is true for CREATE UNIQUE INDEX.
	Unique bool
	// GeneratedStored is true for ADD COLUMN ... GENERATED ... STORED.
	GeneratedStored bool
	// Default is the DEFAULT shape for OpAddColumn.
	Default DefaultKind
	// NewType is the target type for OpAlterColumnType and the column type
	// for OpAddColumn, as the bare grammar type name (e.g. "varchar",
	// "numeric") without the pg_catalog qualification.
	NewType string
	// NewTypeMods are the target type's modifiers (e.g. 50 in varchar(50),
	// 12 and 2 in numeric(12,2)); empty when unconstrained.
	NewTypeMods []int32
	// HasUsing is true for ALTER COLUMN TYPE ... USING <expr>, which always
	// means a conversion, never a binary-coercible relabel.
	HasUsing bool
}

// Describe returns a short operator-facing label for the operation, e.g.
// "ADD COLUMN age" — for plan rendering, never for branching.
func (o Op) Describe() string {
	switch o.Kind {
	case OpCreateTable:
		return "CREATE TABLE"
	case OpAddColumn:
		return "ADD COLUMN " + o.Column
	case OpDropColumn:
		return "DROP COLUMN " + o.Column
	case OpAlterColumnType:
		return "ALTER COLUMN " + o.Column + " TYPE " + o.NewType
	case OpSetDefault:
		return "ALTER COLUMN " + o.Column + " SET DEFAULT"
	case OpDropDefault:
		return "ALTER COLUMN " + o.Column + " DROP DEFAULT"
	case OpSetNotNull:
		return "ALTER COLUMN " + o.Column + " SET NOT NULL"
	case OpDropNotNull:
		return "ALTER COLUMN " + o.Column + " DROP NOT NULL"
	case OpSetColumnOptions:
		return "ALTER COLUMN " + o.Column + " SET options"
	case OpRenameColumn:
		return "RENAME COLUMN " + o.Column + " TO " + o.Name
	case OpRenameTable:
		return "RENAME TO " + o.Name
	case OpRenameIndex:
		return "RENAME INDEX TO " + o.Name
	case OpSetSchema:
		return "SET SCHEMA " + o.Name
	case OpSetTablespace:
		return "SET TABLESPACE " + o.Name
	case OpSetRelOptions:
		return "SET storage parameters"
	case OpAddConstraint:
		return "ADD CONSTRAINT " + o.Name
	case OpValidateConstraint:
		return "VALIDATE CONSTRAINT " + o.Name
	case OpDropConstraint:
		return "DROP CONSTRAINT " + o.Name
	case OpAttachPartition:
		return "ATTACH PARTITION"
	case OpDetachPartition:
		return "DETACH PARTITION"
	case OpCreateIndex:
		return "CREATE INDEX " + o.Name
	case OpDropIndex:
		return "DROP INDEX " + o.Name
	case OpReindex:
		return "REINDEX " + o.Name
	default:
		return "unrecognized operation"
	}
}

// ParseOps parses one SQL statement and returns its typed operations. An
// ALTER TABLE yields one Op per subcommand; every other supported statement
// yields exactly one. Statements and subcommands the engine does not
// recognize come back as OpUnrecognized — never an error — so the classifier can
// refuse them with context. A parse failure is surfaced to the caller.
func ParseOps(sql string) ([]Op, error) {
	tree, err := pgquery.Parse(sql)
	if err != nil {
		return nil, fmt.Errorf("parse statement: %w", err)
	}
	if n := len(tree.GetStmts()); n != 1 {
		return nil, fmt.Errorf("%w: got %d", ErrNotOneStatement, n)
	}
	node := tree.GetStmts()[0].GetStmt()
	switch {
	case node.GetAlterTableStmt() != nil:
		alter := node.GetAlterTableStmt()
		if alter.GetObjtype() != pganalyze.ObjectType_OBJECT_TABLE {
			return []Op{{Kind: OpUnrecognized}}, nil
		}
		ops := make([]Op, 0, len(alter.GetCmds()))
		for _, cmd := range alter.GetCmds() {
			ops = append(ops, alterTableOp(cmd.GetAlterTableCmd()))
		}
		return ops, nil
	case node.GetCreateStmt() != nil:
		return []Op{{Kind: OpCreateTable}}, nil
	case node.GetIndexStmt() != nil:
		idx := node.GetIndexStmt()
		return []Op{{
			Kind:       OpCreateIndex,
			Name:       idx.GetIdxname(),
			Concurrent: idx.GetConcurrent(),
			Unique:     idx.GetUnique(),
		}}, nil
	case node.GetDropStmt() != nil:
		drop := node.GetDropStmt()
		if drop.GetRemoveType() != pganalyze.ObjectType_OBJECT_INDEX {
			return []Op{{Kind: OpUnrecognized}}, nil
		}
		return []Op{{Kind: OpDropIndex, Concurrent: drop.GetConcurrent()}}, nil
	case node.GetReindexStmt() != nil:
		re := node.GetReindexStmt()
		return []Op{{
			Kind:       OpReindex,
			Name:       re.GetRelation().GetRelname(),
			Concurrent: reindexConcurrent(re),
		}}, nil
	case node.GetRenameStmt() != nil:
		return []Op{renameOp(node.GetRenameStmt())}, nil
	case node.GetAlterObjectSchemaStmt() != nil:
		alter := node.GetAlterObjectSchemaStmt()
		if alter.GetObjectType() != pganalyze.ObjectType_OBJECT_TABLE {
			return []Op{{Kind: OpUnrecognized}}, nil
		}
		return []Op{{Kind: OpSetSchema, Name: alter.GetNewschema()}}, nil
	default:
		return []Op{{Kind: OpUnrecognized}}, nil
	}
}

// alterTableOp maps one ALTER TABLE subcommand to its Op.
func alterTableOp(cmd *pganalyze.AlterTableCmd) Op {
	switch cmd.GetSubtype() {
	case pganalyze.AlterTableType_AT_AddColumn:
		return addColumnOp(cmd.GetDef().GetColumnDef())
	case pganalyze.AlterTableType_AT_DropColumn:
		return Op{Kind: OpDropColumn, Column: cmd.GetName()}
	case pganalyze.AlterTableType_AT_AlterColumnType:
		def := cmd.GetDef().GetColumnDef()
		name, mods := typeRef(def.GetTypeName())
		return Op{
			Kind:        OpAlterColumnType,
			Column:      cmd.GetName(),
			NewType:     name,
			NewTypeMods: mods,
			// For ALTER COLUMN TYPE the grammar carries the USING
			// expression in the column definition's raw default slot.
			HasUsing: def.GetRawDefault() != nil,
		}
	case pganalyze.AlterTableType_AT_ColumnDefault:
		if cmd.GetDef() == nil {
			return Op{Kind: OpDropDefault, Column: cmd.GetName()}
		}
		return Op{Kind: OpSetDefault, Column: cmd.GetName()}
	case pganalyze.AlterTableType_AT_SetNotNull:
		return Op{Kind: OpSetNotNull, Column: cmd.GetName()}
	case pganalyze.AlterTableType_AT_DropNotNull:
		return Op{Kind: OpDropNotNull, Column: cmd.GetName()}
	case pganalyze.AlterTableType_AT_SetStatistics,
		pganalyze.AlterTableType_AT_SetStorage,
		pganalyze.AlterTableType_AT_SetOptions,
		pganalyze.AlterTableType_AT_ResetOptions:
		return Op{Kind: OpSetColumnOptions, Column: cmd.GetName()}
	case pganalyze.AlterTableType_AT_SetRelOptions,
		pganalyze.AlterTableType_AT_ResetRelOptions:
		return Op{Kind: OpSetRelOptions}
	case pganalyze.AlterTableType_AT_SetTableSpace:
		return Op{Kind: OpSetTablespace, Name: cmd.GetName()}
	case pganalyze.AlterTableType_AT_AddConstraint:
		return addConstraintOp(cmd.GetDef().GetConstraint())
	case pganalyze.AlterTableType_AT_ValidateConstraint:
		return Op{Kind: OpValidateConstraint, Name: cmd.GetName()}
	case pganalyze.AlterTableType_AT_DropConstraint:
		return Op{Kind: OpDropConstraint, Name: cmd.GetName()}
	case pganalyze.AlterTableType_AT_AttachPartition:
		return Op{Kind: OpAttachPartition}
	case pganalyze.AlterTableType_AT_DetachPartition:
		return Op{Kind: OpDetachPartition, Concurrent: cmd.GetDef().GetPartitionCmd().GetConcurrent()}
	default:
		return Op{Kind: OpUnrecognized}
	}
}

// addColumnOp extracts the shape facts of an added column: its DEFAULT
// shape and whether it is a stored generated column. Identity and serial
// columns are reported as expression defaults — their values come from a
// sequence, which fast default cannot cover.
func addColumnOp(def *pganalyze.ColumnDef) Op {
	op := Op{Kind: OpAddColumn, Column: def.GetColname()}
	op.NewType, op.NewTypeMods = typeRef(def.GetTypeName())
	if isSerialType(op.NewType) {
		op.Default = DefaultExpression
	}
	for _, c := range def.GetConstraints() {
		con := c.GetConstraint()
		switch con.GetContype() {
		case pganalyze.ConstrType_CONSTR_DEFAULT:
			if isConstantExpr(con.GetRawExpr()) {
				op.Default = DefaultConstant
			} else {
				op.Default = DefaultExpression
			}
		case pganalyze.ConstrType_CONSTR_IDENTITY:
			op.Default = DefaultExpression
		case pganalyze.ConstrType_CONSTR_GENERATED:
			op.GeneratedStored = true
		}
	}
	return op
}

// addConstraintOp extracts the shape facts of an ADD CONSTRAINT.
func addConstraintOp(con *pganalyze.Constraint) Op {
	op := Op{
		Kind:       OpAddConstraint,
		Name:       con.GetConname(),
		NotValid:   con.GetSkipValidation(),
		UsingIndex: con.GetIndexname() != "",
	}
	switch con.GetContype() {
	case pganalyze.ConstrType_CONSTR_PRIMARY:
		op.Constraint = ConstraintPrimaryKey
	case pganalyze.ConstrType_CONSTR_UNIQUE:
		op.Constraint = ConstraintUnique
	case pganalyze.ConstrType_CONSTR_CHECK:
		op.Constraint = ConstraintCheck
	case pganalyze.ConstrType_CONSTR_FOREIGN:
		op.Constraint = ConstraintForeignKey
	case pganalyze.ConstrType_CONSTR_NOTNULL:
		op.Constraint = ConstraintNotNull
	default:
		op.Constraint = ConstraintUnrecognized
	}
	for _, k := range con.GetKeys() {
		op.Columns = append(op.Columns, k.GetString_().GetSval())
	}
	return op
}

// renameOp maps a RENAME statement (column, table, or index — the grammar
// parses all three as RenameStmt) to its Op.
func renameOp(ren *pganalyze.RenameStmt) Op {
	switch ren.GetRenameType() {
	case pganalyze.ObjectType_OBJECT_COLUMN:
		return Op{Kind: OpRenameColumn, Column: ren.GetSubname(), Name: ren.GetNewname()}
	case pganalyze.ObjectType_OBJECT_TABLE:
		return Op{Kind: OpRenameTable, Name: ren.GetNewname()}
	case pganalyze.ObjectType_OBJECT_INDEX:
		return Op{Kind: OpRenameIndex, Name: ren.GetNewname()}
	default:
		return Op{Kind: OpUnrecognized}
	}
}

// reindexConcurrent reports whether a REINDEX statement carries the
// CONCURRENTLY option (a DefElem in the statement's parameter list).
func reindexConcurrent(re *pganalyze.ReindexStmt) bool {
	for _, p := range re.GetParams() {
		if p.GetDefElem().GetDefname() == "concurrently" {
			return true
		}
	}
	return false
}

// typeRef returns the bare grammar type name (last path element, without
// the pg_catalog qualification) and its integer modifiers.
func typeRef(tn *pganalyze.TypeName) (string, []int32) {
	names := tn.GetNames()
	if len(names) == 0 {
		return "", nil
	}
	name := names[len(names)-1].GetString_().GetSval()
	var mods []int32
	for _, m := range tn.GetTypmods() {
		mods = append(mods, int32(m.GetAConst().GetIval().GetIval()))
	}
	return name, mods
}

// isSerialType reports whether the grammar type name is one of the serial
// pseudo-types, which expand to a sequence-backed default.
func isSerialType(name string) bool {
	switch strings.ToLower(name) {
	case "serial", "serial2", "serial4", "serial8", "smallserial", "bigserial":
		return true
	default:
		return false
	}
}

// isConstantExpr reports whether a DEFAULT expression is a provable
// constant: a literal, possibly wrapped in type casts. Anything else —
// function calls, value functions like CURRENT_TIMESTAMP, expressions —
// is not, and the caller treats it as volatile.
func isConstantExpr(node *pganalyze.Node) bool {
	switch {
	case node.GetAConst() != nil:
		return true
	case node.GetTypeCast() != nil:
		return isConstantExpr(node.GetTypeCast().GetArg())
	default:
		return false
	}
}
