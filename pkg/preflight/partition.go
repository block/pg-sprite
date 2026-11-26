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
	// PartitionCauseConcurrentIndexBuild means a step list attempts a
	// concurrent index build on the parent, which PostgreSQL has no
	// statement for. The front doors send a CREATE INDEX on a partitioned
	// table to the partition-aware build before this check
	// (RoutesToPartitionedIndexBuild), so only a step list that bypasses the
	// routing carries one.
	PartitionCauseConcurrentIndexBuild PartitionRefusalCause = "parent-concurrent-index-build"
	// PartitionCauseBlockingIndexBuild means a step would build an index on
	// the parent while holding a lock that blocks writes to the whole tree.
	// A CREATE INDEX reaches it only when the operator forced the submitted
	// form, which overrides the routing to the partition-aware build.
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
// causes are a capability boundary: the partition-aware build is the
// supported route for an index on a partitioned parent, and these are the
// forms of the same request it does not run as written. Index adoption is by design: no
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
		return "PostgreSQL supports a blocking parent-level index build, but pg-sprite refuses to run it as written because it blocks writes to the whole table; " +
			"a CREATE INDEX on a partitioned table, without the force override, is built partition by partition instead"
	case PartitionCauseConcurrentIndexBuild:
		return "PostgreSQL cannot build parent-level indexes concurrently, so a sequence of steps cannot carry one on a partitioned table; " +
			"a CREATE INDEX on a partitioned table is built partition by partition instead"
	case PartitionCauseConcurrentIndexDrop:
		return "PostgreSQL cannot drop an index of a partitioned table concurrently in any supported version, and a partition's " +
			"own index cannot be dropped alone; the drop is a plain DROP INDEX, which takes ACCESS EXCLUSIVE on the parent, " +
			"every partition, and every partition's index — run it through the imperative front door with the blocking lock " +
			"and the destructive drop both acknowledged for the table"
	default:
		return "pg-sprite refuses an unsupported operation on a partitioned table"
	}
}

// RoutesToPartitionedIndexBuild reports whether the steps planned for one
// statement are the one CREATE INDEX that the partition-aware index build
// carries when the statement's target is a partitioned table: a CREATE
// INDEX in any form, with or without CONCURRENTLY or ONLY. A front door asks
// it after it knows the target is partitioned, and runs the build with the
// statement itself, not with the steps, which are a plain table's substitute
// (a concurrent build) and would fail on a partitioned one. Any other step
// list is false. A step that does not parse is an error, as it is for
// RefusesPartitionedParent.
func RoutesToPartitionedIndexBuild(execSQL []string) (bool, error) {
	if len(execSQL) != 1 {
		return false, nil
	}
	st, err := statement.ParseOne(execSQL[0])
	if err != nil {
		return false, fmt.Errorf("check partitioned index build routing: %w", err)
	}
	return st.Kind() == statement.KindCreateIndex, nil
}

// partitionedNotValidForeignKeyMajor is the first PostgreSQL major that
// accepts ADD FOREIGN KEY ... NOT VALID on a partitioned table.
const partitionedNotValidForeignKeyMajor = 18

// RoutesToPartitionedForeignKeyBuild reports whether a statement whose target
// is a partitioned table, on a server of major version serverMajor, is the
// one ADD FOREIGN KEY that the partitioned foreign key build carries: an
// ALTER TABLE whose only subcommand adds a foreign key without NOT VALID, on
// a server before version 18. A front door asks it of the submitted
// statement, after it knows the target is partitioned and before the steps
// the planner made for a plain table (ADD ... NOT VALID, then VALIDATE, which
// PostgreSQL refuses on a partitioned table before version 18) reach
// RefusesPartitionedParent, and runs the build with the statement itself.
// From version 18 those steps run as they are, so it is false there. A
// statement the build's own admission refuses, such as an unnamed foreign
// key or one with an ON DELETE column list, still routes, so it is refused
// with the build's reason. A submitted NOT VALID foreign key, and a
// statement with any other subcommand beside the foreign key, are false and
// meet RefusesPartitionedParent as before. A statement that does not parse is
// an error, as it is for RefusesPartitionedParent.
func RoutesToPartitionedForeignKeyBuild(serverMajor int, sql string) (bool, error) {
	if serverMajor >= partitionedNotValidForeignKeyMajor {
		return false, nil
	}
	st, err := statement.ParseOne(sql)
	if err != nil {
		return false, fmt.Errorf("check partitioned foreign key build routing: %w", err)
	}
	if st.Kind() != statement.KindAlterTable {
		return false, nil
	}
	ops, err := statement.ParseOps(sql)
	if err != nil {
		return false, fmt.Errorf("check partitioned foreign key build routing: %w", err)
	}
	if len(ops) != 1 {
		return false, nil
	}
	return addsValidatedForeignKey(ops[0]), nil
}

// addsValidatedForeignKey reports whether op adds a foreign key that is
// validated as it is added, that is, without NOT VALID.
func addsValidatedForeignKey(op statement.Op) bool {
	if op.Kind != statement.OpAddConstraint || op.Constraint != statement.ConstraintForeignKey {
		return false
	}
	return !op.NotValid
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
