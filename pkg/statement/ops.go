package statement

import (
	"fmt"
	"strconv"
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
// The shape alone cannot settle whether PostgreSQL rewrites the table: a
// constant still rewrites every row when the column's type is a domain with
// constraints, and a non-volatile expression such as now() does not. The
// server answers that (preflight.AddColumnRewrites); without its answer,
// any expression is treated as volatile, conservatively.
type DefaultKind int

// The default shapes an added column can carry.
const (
	// DefaultNone: no DEFAULT clause.
	DefaultNone DefaultKind = iota
	// DefaultConstant: a literal (possibly type-cast), which the catalog
	// stores unless the column's type forces a rewrite.
	DefaultConstant
	// DefaultExpression: anything else — function calls, identity, serial.
	// The engine does not evaluate volatility offline; without the
	// server's answer it assumes the worst.
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
	// Only reports that an ALTER TABLE names its table with ONLY, so the
	// operation stops at the table and does not recurse into the tables
	// below it.
	Only bool
	// Indexes are the unqualified names of the indexes an OpDropIndex
	// drops, in statement order, so a classifier can match each one
	// without parsing Name's display form, or the one index an
	// ADD CONSTRAINT ... USING INDEX takes over.
	Indexes []string
	// Columns are the plain key columns of an ADD PRIMARY KEY / UNIQUE;
	// empty when the keys are expressions.
	Columns []string
	// IncludeColumns are the INCLUDE columns of an ADD PRIMARY KEY /
	// UNIQUE: carried in the index, not part of the key.
	IncludeColumns []string
	// NullsNotDistinct is true for an ADD UNIQUE NULLS NOT DISTINCT, which
	// rejects a second row whose key is NULL.
	NullsNotDistinct bool
	// Deferrable and InitiallyDeferred are an ADD PRIMARY KEY / UNIQUE's
	// DEFERRABLE and INITIALLY DEFERRED: when the uniqueness check runs.
	Deferrable        bool
	InitiallyDeferred bool
	// IndexTablespace is the USING INDEX TABLESPACE of an ADD PRIMARY KEY
	// / UNIQUE: where its index is built.
	IndexTablespace string
	// IndexStorageParams are the WITH (...) storage parameters of an ADD
	// PRIMARY KEY / UNIQUE's index, in statement order.
	IndexStorageParams []StorageParam
	// IndexOptionsUnrenderable is true when an ADD PRIMARY KEY / UNIQUE's
	// WITH (...) carries a parameter IndexStorageParams cannot hold
	// faithfully: one whose value is not a plain word, number, or string,
	// or holds a backslash.
	IndexOptionsUnrenderable bool
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
	// IfNotExists is true for CREATE TABLE IF NOT EXISTS and
	// CREATE INDEX IF NOT EXISTS.
	IfNotExists bool
	// GeneratedStored is true for ADD COLUMN ... GENERATED ... STORED.
	GeneratedStored bool
	// InlineConstraints are the table-constraint families an added column
	// carries inline (UNIQUE, PRIMARY KEY, REFERENCES, CHECK) — each does
	// the same index build or validation scan as its ADD CONSTRAINT form.
	// An inline constraint the engine does not model is reported as
	// ConstraintUnrecognized so the classifier can refuse it.
	InlineConstraints []ConstraintKind
	// PartitionOf is true for CREATE TABLE ... PARTITION OF, which locks
	// the partitioned parent, not just the new relation.
	PartitionOf bool
	// Inherits is true for CREATE TABLE ... INHERITS, which locks each
	// named parent — an existing relation, resolved via search_path when
	// unqualified. Disjoint from PartitionOf: the grammar carries the
	// partitioned parent in the same clause, but only PARTITION OF sets a
	// partition bound.
	Inherits bool
	// Like is true for a CREATE TABLE with a LIKE clause, which reads an
	// existing source table — resolved via search_path when unqualified.
	Like bool
	// OfType is true for CREATE TABLE ... OF type, which binds the table
	// to an existing composite type — resolved via search_path when
	// unqualified.
	OfType bool
	// ForeignKey is true for a CREATE TABLE that declares a foreign key, in
	// either the column or the table-constraint form; creating it locks
	// each referenced table in SHARE ROW EXCLUSIVE mode.
	ForeignKey bool
	// Default is the DEFAULT shape for OpAddColumn.
	Default DefaultKind
	// NewType is the target type for OpAlterColumnType and the column type
	// for OpAddColumn, as the bare grammar type name (e.g. "varchar",
	// "numeric") without the pg_catalog qualification.
	NewType string
	// NewTypeMods are the target type's modifiers (e.g. 50 in varchar(50),
	// 12 and 2 in numeric(12,2)); empty when unconstrained.
	NewTypeMods []int32
	// NewTypeSchema is the schema the target type is qualified with, empty
	// when it is unqualified or qualified with pg_catalog. A qualified type
	// is a user type that may share a built-in's name.
	NewTypeSchema string
	// NewTypeArray is true when the target type is an array of NewType
	// (varchar(50)[], text ARRAY).
	NewTypeArray bool
	// HasUsing is true for ALTER COLUMN TYPE ... USING <expr>, which always
	// means a conversion, never a binary-coercible relabel.
	HasUsing bool
}

// Describe returns a short operator-facing label for the operation, e.g.
// "ADD COLUMN age" — for plan rendering, never for branching.
func (o Op) Describe() string {
	switch o.Kind {
	case OpCreateTable:
		if o.PartitionOf {
			return "CREATE TABLE PARTITION OF"
		}
		return "CREATE TABLE"
	case OpAddColumn:
		return "ADD COLUMN " + o.Column
	case OpDropColumn:
		return "DROP COLUMN " + o.Column
	case OpAlterColumnType:
		return "ALTER COLUMN " + o.Column + " TYPE " + o.formatNewType()
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
		// An unnamed CREATE INDEX (the server auto-names it) labels
		// without a trailing name; TrimSpace keeps the label clean.
		return strings.TrimSpace("CREATE INDEX " + o.Name)
	case OpDropIndex:
		return strings.TrimSpace("DROP INDEX " + o.Name)
	case OpReindex:
		return strings.TrimSpace("REINDEX " + o.Name)
	default:
		return "unrecognized operation"
	}
}

// hasLikeClause reports whether any table element is a LIKE clause, which
// copies column definitions from an existing source table.
func hasLikeClause(create *pganalyze.CreateStmt) bool {
	for _, elt := range create.GetTableElts() {
		if elt.GetTableLikeClause() != nil {
			return true
		}
	}
	return false
}

// dropIndexNames renders the dropped index names for the operation label:
// each object's qualified name, comma-separated when one statement drops
// several. The name identifies which structure the plan discards, so a
// label without it would leave a destructive decision anonymous.
func dropIndexNames(drop *pganalyze.DropStmt) string {
	names := make([]string, 0, len(drop.GetObjects()))
	for _, obj := range drop.GetObjects() {
		parts := make([]string, 0, len(obj.GetList().GetItems()))
		for _, item := range obj.GetList().GetItems() {
			parts = append(parts, item.GetString_().GetSval())
		}
		names = append(names, strings.Join(parts, "."))
	}
	return strings.Join(names, ", ")
}

// droppedIndexes returns the unqualified name of each index a DROP INDEX
// names: the last part of each object's qualified name.
func droppedIndexes(drop *pganalyze.DropStmt) []string {
	names := make([]string, 0, len(drop.GetObjects()))
	for _, obj := range drop.GetObjects() {
		items := obj.GetList().GetItems()
		if len(items) == 0 {
			continue
		}
		names = append(names, items[len(items)-1].GetString_().GetSval())
	}
	return names
}

// formatNewType renders the target type with its schema, modifiers, and
// array marker — varchar(50), numeric(12,2), app.email, varchar(50)[]. Each
// part can turn a relabel into a rewrite (a widen is not a narrow, a
// varchar is not a varchar array), so a rendering that drops one would
// collapse changes that route in opposite directions.
func (o Op) formatNewType() string {
	name := o.NewType
	if o.NewTypeSchema != "" {
		name = o.NewTypeSchema + "." + name
	}
	if len(o.NewTypeMods) > 0 {
		parts := make([]string, len(o.NewTypeMods))
		for i, m := range o.NewTypeMods {
			parts[i] = strconv.FormatInt(int64(m), 10)
		}
		name += "(" + strings.Join(parts, ",") + ")"
	}
	if o.NewTypeArray {
		name += "[]"
	}
	return name
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
			op := alterTableOp(cmd.GetAlterTableCmd())
			op.Only = !alter.GetRelation().GetInh()
			ops = append(ops, op)
		}
		return ops, nil
	case node.GetCreateStmt() != nil:
		create := node.GetCreateStmt()
		return []Op{{
			Kind:        OpCreateTable,
			PartitionOf: create.GetPartbound() != nil,
			Inherits:    create.GetPartbound() == nil && len(create.GetInhRelations()) > 0,
			Like:        hasLikeClause(create),
			OfType:      create.GetOfTypename() != nil,
			ForeignKey:  len(foreignKeyConstraints(create)) > 0,
			IfNotExists: create.GetIfNotExists(),
		}}, nil
	case node.GetIndexStmt() != nil:
		idx := node.GetIndexStmt()
		return []Op{{
			Kind:        OpCreateIndex,
			Name:        idx.GetIdxname(),
			Concurrent:  idx.GetConcurrent(),
			Unique:      idx.GetUnique(),
			IfNotExists: idx.GetIfNotExists(),
		}}, nil
	case node.GetDropStmt() != nil:
		drop := node.GetDropStmt()
		if drop.GetRemoveType() != pganalyze.ObjectType_OBJECT_INDEX {
			return []Op{{Kind: OpUnrecognized}}, nil
		}
		return []Op{{Kind: OpDropIndex, Name: dropIndexNames(drop), Indexes: droppedIndexes(drop), Concurrent: drop.GetConcurrent()}}, nil
	case node.GetReindexStmt() != nil:
		re := node.GetReindexStmt()
		return []Op{{
			Kind:       OpReindex,
			Name:       re.GetRelation().GetRelname(),
			Concurrent: reindexConcurrently(re),
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
		schema, array := typeQualifiers(def.GetTypeName())
		return Op{
			Kind:          OpAlterColumnType,
			Column:        cmd.GetName(),
			NewType:       name,
			NewTypeMods:   mods,
			NewTypeSchema: schema,
			NewTypeArray:  array,
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
// shape, whether it is a stored generated column, and any inline table
// constraints it carries. Identity and serial columns are reported as
// expression defaults — their values come from a sequence, which fast
// default cannot cover. Nullability clauses and the deferrability
// attributes that modify a preceding FOREIGN KEY add no work of their own
// and are not reported; any constraint family the engine does not model
// is reported as ConstraintUnrecognized so the classifier fails closed.
func addColumnOp(def *pganalyze.ColumnDef) Op {
	op := Op{Kind: OpAddColumn, Column: def.GetColname()}
	op.NewType, op.NewTypeMods = typeRef(def.GetTypeName())
	op.NewTypeSchema, op.NewTypeArray = typeQualifiers(def.GetTypeName())
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
		case pganalyze.ConstrType_CONSTR_NULL,
			pganalyze.ConstrType_CONSTR_NOTNULL,
			pganalyze.ConstrType_CONSTR_ATTR_DEFERRABLE,
			pganalyze.ConstrType_CONSTR_ATTR_NOT_DEFERRABLE,
			pganalyze.ConstrType_CONSTR_ATTR_DEFERRED,
			pganalyze.ConstrType_CONSTR_ATTR_IMMEDIATE:
			// No scan and no index build of their own.
		case pganalyze.ConstrType_CONSTR_UNIQUE:
			op.InlineConstraints = append(op.InlineConstraints, ConstraintUnique)
		case pganalyze.ConstrType_CONSTR_PRIMARY:
			op.InlineConstraints = append(op.InlineConstraints, ConstraintPrimaryKey)
		case pganalyze.ConstrType_CONSTR_FOREIGN:
			op.InlineConstraints = append(op.InlineConstraints, ConstraintForeignKey)
		case pganalyze.ConstrType_CONSTR_CHECK:
			op.InlineConstraints = append(op.InlineConstraints, ConstraintCheck)
		default:
			op.InlineConstraints = append(op.InlineConstraints, ConstraintUnrecognized)
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
	if index := con.GetIndexname(); index != "" {
		op.Indexes = []string{index}
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
	if op.Constraint == ConstraintPrimaryKey || op.Constraint == ConstraintUnique {
		keyIndexAttributes(&op, con)
	}
	return op
}

// keyIndexAttributes records what an ADD PRIMARY KEY / UNIQUE declares
// beyond its key columns: everything an online sequence that builds the
// index first has to carry for the result to be the same constraint.
func keyIndexAttributes(op *Op, con *pganalyze.Constraint) {
	for _, c := range con.GetIncluding() {
		op.IncludeColumns = append(op.IncludeColumns, c.GetString_().GetSval())
	}
	op.NullsNotDistinct = con.GetNullsNotDistinct()
	op.Deferrable = con.GetDeferrable()
	op.InitiallyDeferred = con.GetInitdeferred()
	op.IndexTablespace = con.GetIndexspace()
	for _, o := range con.GetOptions() {
		param, ok := storageParam(o.GetDefElem())
		if !ok {
			op.IndexOptionsUnrenderable = true
			continue
		}
		op.IndexStorageParams = append(op.IndexStorageParams, param)
	}
}

// StorageParam is one WITH (...) storage parameter. The server reads every
// parameter's value as text, so Value holds that text whatever literal
// spelled it; HasValue is false for a bare name, which the server reads as
// true.
type StorageParam struct {
	Name     string
	Value    string
	HasValue bool
}

// plainWord returns the word a type name spells when it is one unqualified
// name with no modifiers or array bounds, and "" otherwise.
func plainWord(tn *pganalyze.TypeName) string {
	if tn == nil || len(tn.GetNames()) != 1 || len(tn.GetTypmods()) > 0 || len(tn.GetArrayBounds()) > 0 ||
		tn.GetSetof() || tn.GetPctType() {
		return ""
	}
	return tn.GetNames()[0].GetString_().GetSval()
}

// storageParam reads one WITH (...) element, reporting false for one a
// StorageParam cannot hold faithfully.
func storageParam(de *pganalyze.DefElem) (StorageParam, bool) {
	if de == nil {
		return StorageParam{}, false
	}
	if de.GetDefnamespace() != "" {
		// A StorageParam has no namespace to carry, and rendering the bare
		// name would set a different option than the one written.
		return StorageParam{}, false
	}
	param := StorageParam{Name: de.GetDefname()}
	arg := de.GetArg()
	switch {
	case arg == nil:
		return param, true
	case arg.GetString_() != nil:
		param.Value = arg.GetString_().GetSval()
	case arg.GetInteger() != nil:
		param.Value = strconv.FormatInt(int64(arg.GetInteger().GetIval()), 10)
	case arg.GetFloat() != nil:
		param.Value = arg.GetFloat().GetFval()
	case arg.GetBoolean() != nil:
		param.Value = strconv.FormatBool(arg.GetBoolean().GetBoolval())
	case plainWord(arg.GetTypeName()) != "":
		// The grammar parses an unreserved bare word such as off as a
		// type name; the server reads it back as the word.
		param.Value = plainWord(arg.GetTypeName())
	default:
		return StorageParam{}, false
	}
	if strings.Contains(param.Value, `\`) {
		// A backslash reads differently under standard_conforming_strings
		// off, and no storage parameter takes one.
		return StorageParam{}, false
	}
	param.HasValue = true
	return param, true
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

// typeQualifiers returns what typeRef leaves out of a grammar type name: the
// schema it is qualified with (empty for pg_catalog, which the grammar adds
// to keyword types such as varchar and numeric) and whether it is an array.
func typeQualifiers(tn *pganalyze.TypeName) (string, bool) {
	array := len(tn.GetArrayBounds()) > 0
	names := tn.GetNames()
	if len(names) < 2 {
		return "", array
	}
	schema := names[len(names)-2].GetString_().GetSval()
	if schema == "pg_catalog" {
		return "", array
	}
	return schema, array
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
