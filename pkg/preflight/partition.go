package preflight

import (
	"fmt"

	"github.com/block/pg-sprite/pkg/statement"
)

// UnsupportedPartitionedParentError reports that a routed plan would build
// an index on a partitioned parent. PostgreSQL does not support concurrent
// parent-level index builds, and pg-sprite does not yet implement the
// partition-aware sequence needed to replace one.
type UnsupportedPartitionedParentError struct{}

// Error implements the error interface.
func (*UnsupportedPartitionedParentError) Error() string {
	return "parent-level concurrent index builds are unsupported by PostgreSQL; " +
		"the partition-aware CREATE INDEX ON ONLY, per-partition CREATE INDEX CONCURRENTLY, " +
		"and ATTACH PARTITION flow is not yet supported"
}

// CheckPartitionSupport verifies that the routed execution steps are safe
// for the target's relation kind. Ordinary tables and leaf partitions pass
// unchanged. Partitioned parents are refused only when a step builds an
// index; supported in-place parent ALTER TABLE operations remain available.
func CheckPartitionSupport(table PreflightedTable, execSQL []string) error {
	if !table.Partitioned() {
		return nil
	}
	for _, sql := range execSQL {
		st, err := statement.ParseOne(sql)
		if err != nil {
			return fmt.Errorf("check partitioned-parent support: %w", err)
		}
		if st.BuildsIndex() {
			return &UnsupportedPartitionedParentError{}
		}
	}
	return nil
}
