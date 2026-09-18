package statement

import (
	"errors"
	"fmt"
	"reflect"
)

var (
	// ErrNotAlterTable is returned when relation retargeting is requested for
	// anything other than a single ALTER TABLE.
	ErrNotAlterTable = errors.New("statement is not ALTER TABLE")
	// ErrRetargetMismatch is returned when the retargeted statement, re-parsed,
	// differs from the gated statement in anything but its target relation.
	ErrRetargetMismatch = errors.New("retargeted statement differs beyond its target relation")
)

// RetargetRelation returns sql with its ALTER TABLE target relation replaced
// by schema.table and reprinted through the PostgreSQL deparser. It is the
// one AST edit the copy-and-swap route permits: the gated statement names
// the source table, and the shadow builder needs the same operations against
// the empty shadow. Only the target RangeVar changes — every other node,
// including a REFERENCES or expression that names the source table, is left
// exactly as the user wrote it.
func RetargetRelation(sql, schema, table string) (string, error) {
	node, err := parseSingle(sql)
	if err != nil {
		return "", err
	}
	alter := node.GetAlterTableStmt()
	if alter == nil || alter.GetRelation() == nil {
		return "", ErrNotAlterTable
	}
	alter.Relation.Schemaname = schema
	alter.Relation.Relname = table
	return deparseOne(node)
}

// SameOpsExceptTarget proves that two ALTER TABLE statements parse to the
// same operations, permitting only their target relations to differ. The
// shadow builder runs it on the gated statement and its retargeted form
// before executing anything on the shadow (ST-7 with the shadow as the sole
// permitted target).
func SameOpsExceptTarget(gated, retargeted string) error {
	gatedNode, err := parseSingle(gated)
	if err != nil {
		return err
	}
	retargetedNode, err := parseSingle(retargeted)
	if err != nil {
		return err
	}
	if gatedNode.GetAlterTableStmt() == nil || retargetedNode.GetAlterTableStmt() == nil {
		return ErrNotAlterTable
	}
	gatedOps, err := ParseOps(gated)
	if err != nil {
		return err
	}
	retargetedOps, err := ParseOps(retargeted)
	if err != nil {
		return err
	}
	// INV: ST-7
	if !reflect.DeepEqual(gatedOps, retargetedOps) {
		return fmt.Errorf("%w: %q vs %q", ErrRetargetMismatch, gated, retargeted)
	}
	return nil
}
