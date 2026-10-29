package suggest

import "github.com/block/pg-sprite/pkg/statement"

// ReAddedColumn reports the first column one statement both drops and adds
// back under the same name, in statement order: the drop-and-re-add shape
// that discards the column's values when the intent is usually a change of
// type. ok is false when no dropped column is added back. A destructive-change
// refusal carries [GuidanceAlterColumnTypeInPlace] for it, so the operator
// sees how to keep the values before acknowledging their loss.
func ReAddedColumn(ops []statement.Op) (column string, ok bool) {
	dropped := map[string]bool{}
	for _, op := range ops {
		if op.Kind == statement.OpDropColumn {
			dropped[op.Column] = true
		}
	}
	for _, op := range ops {
		if op.Kind == statement.OpAddColumn && dropped[op.Column] {
			return op.Column, true
		}
	}
	return "", false
}
