package preflight

import (
	"fmt"

	"github.com/block/pg-sprite/pkg/refusal"
	"github.com/block/pg-sprite/pkg/statement"
)

// PartitionRefusalCause identifies which unsupported shape triggered a
// partitioned-parent refusal. The zero value means the steps are supported.
type PartitionRefusalCause string

const (
	// PartitionCauseConcurrentIndexBuild means a step attempts a concurrent
	// index build on the parent.
	PartitionCauseConcurrentIndexBuild PartitionRefusalCause = "parent-concurrent-index-build"
	// PartitionCauseBlockingIndexBuild means a step would build an index on
	// the parent while holding ACCESS EXCLUSIVE.
	PartitionCauseBlockingIndexBuild PartitionRefusalCause = "parent-blocking-index-build"
	// PartitionCauseIndexAdoption means a step adopts an existing index as a
	// primary-key or unique constraint on the parent.
	PartitionCauseIndexAdoption PartitionRefusalCause = "parent-index-adoption"
	// PartitionCauseNotValidForeignKey means a step adds a NOT VALID
	// foreign key, which the server version cannot do on a partitioned
	// table.
	PartitionCauseNotValidForeignKey PartitionRefusalCause = "parent-not-valid-foreign-key"
	// PartitionCauseConcurrentIndexDrop means a step drops an index of the
	// parent concurrently, which PostgreSQL rejects for a partitioned index.
	PartitionCauseConcurrentIndexDrop PartitionRefusalCause = "parent-concurrent-index-drop"
)

// PartitionRefusalCauses returns the closed set of partitioned-parent
// refusal causes, so documentation and consumers can enumerate them instead
// of maintaining their own list.
func PartitionRefusalCauses() []PartitionRefusalCause {
	return []PartitionRefusalCause{
		PartitionCauseConcurrentIndexBuild,
		PartitionCauseBlockingIndexBuild,
		PartitionCauseIndexAdoption,
		PartitionCauseNotValidForeignKey,
		PartitionCauseConcurrentIndexDrop,
	}
}

// Class is the routing class of the cause, from docs/refusal-classes.md:
// what kind of boundary the consumer is looking at. The two index-build
// causes are a capability boundary — the partition-aware concurrent index
// flow is a planned engine capability, and refusing the blocking substitute
// is the policy half of the same gap. Index adoption is by design: no
// supported server adopts an index as a constraint on a partitioned parent,
// so waiting for an engine release waits for nothing; the same holds for a
// concurrent drop of a partitioned index. The NOT VALID foreign
// key is environmental: the same statement runs on a newer server, so the
// action that unblocks it is a server upgrade. The class is a triage
// decision; the cause's row in the document decides what the consumer does
// next. The zero cause, and an unknown one, have no class.
func (c PartitionRefusalCause) Class() refusal.Class {
	switch c {
	case PartitionCauseConcurrentIndexBuild, PartitionCauseBlockingIndexBuild:
		return refusal.ClassCapabilityBoundary
	case PartitionCauseIndexAdoption, PartitionCauseConcurrentIndexDrop:
		return refusal.ClassByDesign
	case PartitionCauseNotValidForeignKey:
		return refusal.ClassEnvironmental
	default:
		return ""
	}
}

// UnsupportedPartitionedParentError reports that an execution plan contains
// a step pg-sprite cannot safely run on a partitioned parent. Its rendered
// message is a fixed English sentence with no interpolated identifiers or
// server text, so orchestrator-facing surfaces may render it verbatim. This
// is a deliberate property to preserve.
type UnsupportedPartitionedParentError struct {
	// Cause is the unsupported shape that triggered the refusal.
	Cause PartitionRefusalCause
}

// Error implements the error interface.
func (e *UnsupportedPartitionedParentError) Error() string {
	switch e.Cause {
	case PartitionCauseNotValidForeignKey:
		return "PostgreSQL before version 18 cannot add a NOT VALID foreign key on a partitioned table; " +
			"pg-sprite refuses the plan rather than failing mid-change"
	case PartitionCauseIndexAdoption:
		return "PostgreSQL does not support ALTER TABLE ... ADD CONSTRAINT ... USING INDEX on partitioned tables in any supported version"
	case PartitionCauseBlockingIndexBuild:
		return "PostgreSQL supports a blocking parent-level index build, but pg-sprite refuses to run it under ACCESS EXCLUSIVE; " +
			"the partition-aware concurrent flow is not yet supported"
	case PartitionCauseConcurrentIndexBuild:
		return "PostgreSQL cannot build parent-level indexes concurrently; pg-sprite does not yet support " +
			"the partition-aware CREATE INDEX ON ONLY, per-partition CREATE INDEX CONCURRENTLY, and ATTACH PARTITION flow"
	case PartitionCauseConcurrentIndexDrop:
		return "PostgreSQL cannot drop an index of a partitioned table concurrently in any supported version, and a partition's " +
			"own index cannot be dropped alone; the drop is a plain DROP INDEX, which takes ACCESS EXCLUSIVE on the parent, " +
			"every partition, and every partition's index — run it through the imperative front door with the blocking lock " +
			"and the destructive drop both acknowledged for the table"
	default:
		return "pg-sprite refuses an unsupported operation on a partitioned table"
	}
}

// CheckPartitionSupport verifies that the execution steps are safe
// for the target's relation kind. Ordinary tables and leaf partitions pass
// unchanged. Supported in-place parent ALTER TABLE operations remain available.
func CheckPartitionSupport(table PreflightedTable, serverMajor int, execSQL []string) error {
	if !table.Partitioned() {
		return nil
	}
	cause, err := RefusesPartitionedParent(serverMajor, execSQL)
	if err != nil {
		return err
	}
	if cause != "" {
		return &UnsupportedPartitionedParentError{Cause: cause}
	}
	return nil
}

// RefusesPartitionedParent reports the cause that makes steps unsupported on
// a partitioned parent, or the zero value when they are supported. It is the
// shared static policy used by preflight, plan reporting, and executor
// admission.
func RefusesPartitionedParent(serverMajor int, execSQL []string) (PartitionRefusalCause, error) {
	for _, sql := range execSQL {
		st, err := statement.ParseOne(sql)
		if err != nil {
			return "", fmt.Errorf("check partitioned-parent support: %w", err)
		}
		// INV: RF-6 — no partitioned-parent sequence starts when pg-sprite
		// cannot complete it without an ACCESS EXCLUSIVE index build, an
		// unsupported index adoption, a server-version-unsupported NOT
		// VALID foreign key, or a concurrent drop of a partitioned index.
		ops, err := statement.ParseOps(sql)
		if err != nil {
			return "", fmt.Errorf("check partitioned-parent operations: %w", err)
		}
		for _, op := range ops {
			if op.Kind == statement.OpAddConstraint && op.UsingIndex {
				return PartitionCauseIndexAdoption, nil
			}
			if serverMajor < 18 && op.Kind == statement.OpAddConstraint &&
				op.Constraint == statement.ConstraintForeignKey && op.NotValid {
				return PartitionCauseNotValidForeignKey, nil
			}
		}
		// Every index of a partitioned parent is a partitioned index, which
		// PostgreSQL refuses to drop concurrently.
		if st.Kind() == statement.KindDropIndex && st.Concurrent() {
			return PartitionCauseConcurrentIndexDrop, nil
		}
		if st.BuildsIndex() {
			if st.Concurrent() {
				return PartitionCauseConcurrentIndexBuild, nil
			}
			return PartitionCauseBlockingIndexBuild, nil
		}
	}
	return "", nil
}
