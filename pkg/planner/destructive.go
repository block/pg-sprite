package planner

import "github.com/block/pg-sprite/pkg/statement"

// destructiveOp reports whether an operation shape discards live
// structure. A drop is destructive regardless of how it routes: a dropped
// column discards data, a dropped constraint discards a guarantee the
// schema was providing — and dropping NOT NULL discards exactly the same
// guarantee its constraint form would, so it is marked identically — and a
// dropped index discards a structure that is expensive to rebuild (and,
// for a unique index, the uniqueness guarantee). Dropping a DEFAULT is
// deliberately not destructive: a default guarantees nothing about
// existing rows and is recreated by a metadata-only statement.
//
// Both front doors' destructive gates rest on this answer, and the core
// has no backstop for it (SAFETY.md), so a shape nobody has classified is
// treated as destructive: it is refused without the acknowledgement rather
// than run unguarded.
func destructiveOp(kind statement.OpKind) bool {
	destructive, _ := classifyDestructive(kind)
	return destructive
}

// classifyDestructive is the explicit decision for every shape in
// statement.OpKinds(). classified is false only for a shape this switch
// does not name, which TestEveryOpKindIsClassifiedForDestructiveness
// rejects, so a shape added to the parser cannot ship without a decision.
func classifyDestructive(kind statement.OpKind) (destructive, classified bool) {
	switch kind {
	case statement.OpDropColumn,
		statement.OpDropConstraint,
		statement.OpDropIndex,
		statement.OpDropNotNull:
		return true, true
	case statement.OpUnrecognized,
		statement.OpCreateTable,
		statement.OpAddColumn,
		statement.OpAlterColumnType,
		statement.OpSetDefault,
		statement.OpDropDefault,
		statement.OpSetNotNull,
		statement.OpSetColumnOptions,
		statement.OpRenameColumn,
		statement.OpRenameTable,
		statement.OpRenameIndex,
		statement.OpSetSchema,
		statement.OpSetTablespace,
		statement.OpSetRelOptions,
		statement.OpAddConstraint,
		statement.OpValidateConstraint,
		statement.OpAttachPartition,
		statement.OpDetachPartition,
		statement.OpCreateIndex,
		statement.OpReindex:
		return false, true
	default:
		return true, false
	}
}
