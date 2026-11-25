package executor_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/executor"
)

func TestInvariantViolationAliasesDBConn(t *testing.T) {
	assert.ErrorIs(t, dbconn.ErrInvariantViolation, executor.ErrInvariantViolation)
}

func TestOutcomeCodeMapsTypedOutcomes(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want executor.Code
	}{
		{name: "row security review changed", err: fmt.Errorf("review: %w", executor.ErrRowSecurityPlanChanged), want: executor.CodeRowSecurityPlanChanged},
		{name: "row security unknown commit", err: &executor.RowSecurityOutcomeUnknownError{Err: errors.New("connection lost")}, want: executor.CodeRowSecurityOutcomeUnknown},
		{name: "nil error has no code", err: nil, want: executor.Code("")},
		{
			name: "lock budget",
			err:  &executor.BudgetError{Cause: executor.CauseLock, Budget: time.Second},
			want: executor.CodeBudgetLockExceeded,
		},
		{
			name: "statement budget",
			err:  &executor.BudgetError{Cause: executor.CauseStatement, Budget: time.Second},
			want: executor.CodeBudgetStatementExceeded,
		},
		{
			name: "own invalid leftover",
			err:  &executor.InvalidIndexError{Schema: "s", Index: "i", Cleanup: executor.ErrBuildLeftInvalidIndex},
			want: executor.CodeInvalidIndexOwnLeftover,
		},
		{
			name: "own invalid leftover of a build that failed on duplicate rows",
			err: &executor.InvalidIndexError{Schema: "s", Index: "i", Table: "t",
				Build: &pgconn.PgError{Code: "23505"}, Cleanup: executor.ErrBuildLeftInvalidIndex},
			want: executor.CodeDataFault,
		},
		{
			name: "own invalid leftover of a build an index expression failed on a row",
			err: &executor.InvalidIndexError{Schema: "s", Index: "i", Table: "t",
				Build: fmt.Errorf("build: %w", &pgconn.PgError{Code: "22012"}), Cleanup: executor.ErrBuildLeftInvalidIndex},
			want: executor.CodeDataFault,
		},
		{
			name: "own invalid leftover of a build that failed outside the data classes",
			err: &executor.InvalidIndexError{Schema: "s", Index: "i", Table: "t",
				Build: &pgconn.PgError{Code: "42P07"}, Cleanup: executor.ErrBuildLeftInvalidIndex},
			want: executor.CodeInvalidIndexOwnLeftover,
		},
		{
			// Two concurrent builds that pick one index name in one schema
			// collide in pg_class's unique name index: a 23505 that names a
			// catalog relation, not the table's rows.
			name: "a build that lost a catalog name collision to another table's index",
			err: &executor.InvalidIndexError{Schema: "s", Index: "i", Table: "other",
				Build: &pgconn.PgError{Code: "23505", SchemaName: "pg_catalog", TableName: "pg_class",
					ConstraintName: "pg_class_relname_nsp_index"},
				Cleanup: executor.ErrInvalidIndexOnOtherTable},
			want: executor.CodeInvalidIndexOtherTable,
		},
		{
			name: "invalid index build in flight",
			err:  &executor.InvalidIndexError{Schema: "s", Index: "i", BuilderPID: 42, Cleanup: executor.ErrInvalidIndexBuildInFlight},
			want: executor.CodeInvalidIndexBuildInFlight,
		},
		{
			name: "abandoned invalid index",
			err:  &executor.InvalidIndexError{Schema: "s", Index: "i", Table: "t", Cleanup: executor.ErrAbandonedInvalidIndex},
			want: executor.CodeInvalidIndexAbandoned,
		},
		{
			name: "invalid index on another table",
			err:  &executor.InvalidIndexError{Schema: "s", Index: "i", Table: "u", Cleanup: executor.ErrInvalidIndexOnOtherTable},
			want: executor.CodeInvalidIndexOtherTable,
		},
		{
			name: "invalid index with an unobservable builder",
			err:  &executor.InvalidIndexError{Schema: "s", Index: "i", Table: "t", Cleanup: executor.ErrInvalidIndexBuilderUnobservable},
			want: executor.CodeInvalidIndexBuilderUnobservable,
		},
		{
			name: "invalid index the server will not drop concurrently",
			err:  &executor.InvalidIndexError{Schema: "s", Index: "i", Table: "t", Cleanup: executor.ErrInvalidIndexNotDroppable},
			want: executor.CodeInvalidIndexNotDroppable,
		},
		{
			name: "unproven invalid index",
			err:  &executor.InvalidIndexError{Schema: "s", Index: "i", Cleanup: executor.ErrTargetIdentityChanged},
			want: executor.CodeInvalidIndexUnproven,
		},
		{name: "cancelled by caller", err: executor.ErrCancelledByCaller, want: executor.CodeCancelledByCaller},
		{name: "blocking outcome unknown", err: &executor.BlockingOutcomeUnknownError{Err: errors.New("lost")}, want: executor.CodeBlockingOutcomeUnknown},
		{name: "commit outcome unknown", err: &executor.CommitOutcomeUnknownError{Err: errors.New("connection reset")}, want: executor.CodeCommitOutcomeUnknown},
		{name: "drop outcome unknown", err: &executor.DropOutcomeUnknownError{Err: errors.New("connection reset")}, want: executor.CodeDropOutcomeUnknown},
		{
			name: "commit outcome unknown at a sequence step carries its own code",
			err: &executor.SequenceStepError{
				Step: 2, Total: 3, Kind: executor.StepBrief, SQL: "ALTER TABLE s.t ADD c int",
				Err: &executor.CommitOutcomeUnknownError{Err: context.Canceled},
			},
			want: executor.CodeCommitOutcomeUnknown,
		},
		{name: "invalid blocking budget", err: executor.ErrInvalidBlockingBudget, want: executor.CodeInvalidBlockingBudget},
		{name: "unsupported accepted blocking", err: executor.ErrUnsupportedAcceptedBlocking, want: executor.CodeUnsupportedAcceptedBlocking},
		{
			name: "abandonment unproven through to removal",
			err:  &executor.InvalidIndexError{Schema: "s", Index: "i", Cleanup: executor.ErrAbandonmentUnproven},
			want: executor.CodeInvalidIndexUnproven,
		},
		{name: "cancelled externally", err: executor.ErrCancelledExternally, want: executor.CodeCancelledExternally},
		{name: "empty sequence", err: executor.ErrEmptySequence, want: executor.CodeEmptySequence},
		{name: "unsupported sequence step", err: executor.ErrUnsupportedSequenceStep, want: executor.CodeUnsupportedSequenceStep},
		{name: "not a concurrent build", err: executor.ErrNotConcurrentIndexBuild, want: executor.CodeNotConcurrentIndexBuild},
		{name: "unnamed index", err: executor.ErrUnnamedIndex, want: executor.CodeUnnamedIndex},
		{name: "unqualified table", err: executor.ErrUnqualifiedTable, want: executor.CodeUnqualifiedTable},
		{name: "if not exists", err: executor.ErrIfNotExistsUnsupported, want: executor.CodeIfNotExistsUnsupported},
		{name: "create collision", err: executor.ErrCreateCollision, want: executor.CodeCreateCollision},
		{name: "create name mismatch", err: executor.ErrCreateNameMismatch, want: executor.CodeCreateNameMismatch},
		{name: "create names unverified", err: executor.ErrCreateNamesUnverified, want: executor.CodeCreateNamesUnverified},
		{
			name: "create names unverified outranks the read's own cause",
			err:  fmt.Errorf("%w: app.t: %w", executor.ErrCreateNamesUnverified, executor.ErrCancelledByCaller),
			want: executor.CodeCreateNamesUnverified,
		},
		{
			name: "typed create name mismatch at step 1",
			err: &executor.SequenceStepError{
				Step: 1, Total: 2, Kind: executor.StepBrief, SQL: "CREATE TABLE s.t (id int PRIMARY KEY)",
				Err: &executor.CreateNameMismatchError{Schema: "s", Table: "t", Missing: []string{"t_pkey"}, Unclaimed: []string{"t_pkey1"}},
			},
			want: executor.CodeCreateNameMismatch,
		},
		{name: "create owner mismatch", err: executor.ErrCreateOwnerMismatch, want: executor.CodeCreateOwnerMismatch},
		{
			name: "typed create owner mismatch at step 1",
			err: &executor.SequenceStepError{
				Step: 1, Total: 2, Kind: executor.StepBrief, SQL: "CREATE TABLE s.t (id int PRIMARY KEY)",
				Err: &executor.CreateOwnerMismatchError{Expected: "owner", Actual: "engine"},
			},
			want: executor.CodeCreateOwnerMismatch,
		},
		{name: "create owner unverified", err: executor.ErrCreateOwnerUnverified, want: executor.CodeCreateOwnerUnverified},
		{
			name: "create owner unverified outranks the read's own cause",
			err:  fmt.Errorf("%w: app.t: %w", executor.ErrCreateOwnerUnverified, executor.ErrCancelledByCaller),
			want: executor.CodeCreateOwnerUnverified,
		},
		{name: "duplicate create name", err: executor.ErrDuplicateCreateName, want: executor.CodeDuplicateCreateName},
		{name: "partition of", err: executor.ErrPartitionOfUnsupported, want: executor.CodePartitionOfUnsupported},
		{name: "unsupported create step", err: executor.ErrUnsupportedCreateStep, want: executor.CodeUnsupportedCreateStep},
		{name: "pool too small", err: executor.ErrPoolTooSmall, want: executor.CodePoolTooSmall},
		{name: "not create index", err: executor.ErrNotCreateIndex, want: executor.CodeNotCreateIndex},
		{name: "not partitioned table", err: fmt.Errorf("target app.t: %w", executor.ErrNotPartitionedTable), want: executor.CodeNotPartitionedTable},
		{name: "partitioned index name taken", err: fmt.Errorf("app.idx: %w", executor.ErrPartitionedIndexNameTaken), want: executor.CodePartitionedIndexNameTaken},
		{name: "foreign table partition", err: fmt.Errorf("app.t: %w", executor.ErrForeignTablePartition), want: executor.CodeForeignTablePartition},
		{name: "unique index partition key", err: fmt.Errorf("app.t: %w", executor.ErrUniqueIndexPartitionKey), want: executor.CodeUniqueIndexPartitionKey},
		{name: "table lock unconfirmed", err: fmt.Errorf("app.t: %w", executor.ErrTableLockUnconfirmed), want: executor.CodeTableLockUnconfirmed},
		{name: "partitioned foreign key name taken", err: fmt.Errorf("app.t_p1: %w", executor.ErrPartitionedForeignKeyNameTaken), want: executor.CodePartitionedForeignKeyNameTaken},
		{name: "partitioned foreign key conflict", err: fmt.Errorf("app.t_p1: %w", executor.ErrPartitionedForeignKeyConflict), want: executor.CodePartitionedForeignKeyConflict},
		{name: "partitioned foreign key referenced table", err: fmt.Errorf("app.r: %w", executor.ErrPartitionedForeignKeyReferencedTable), want: executor.CodePartitionedForeignKeyReferencedTable},
		{name: "not add foreign key", err: fmt.Errorf("%w: got CREATE INDEX", executor.ErrNotAddForeignKey), want: executor.CodeNotAddForeignKey},
		{name: "partitioned foreign key unsupported shape", err: fmt.Errorf("%w: NOT VALID", executor.ErrPartitionedForeignKeyUnsupportedShape), want: executor.CodePartitionedForeignKeyUnsupportedShape},
		{name: "partitioned foreign key parent conflict", err: fmt.Errorf("app.t: %w", executor.ErrPartitionedForeignKeyParentConflict), want: executor.CodePartitionedForeignKeyParentConflict},
		{name: "partitioned foreign key sub-partitioned", err: fmt.Errorf("app.t: %w", executor.ErrPartitionedForeignKeySubPartitioned), want: executor.CodePartitionedForeignKeySubPartitioned},
		{name: "partitioned foreign key not valid supported", err: fmt.Errorf("PostgreSQL 18: %w", executor.ErrPartitionedForeignKeyNotValidSupported), want: executor.CodePartitionedForeignKeyNotValidSupported},
		{name: "partitioned foreign key unsettled", err: fmt.Errorf("app.t: %w", executor.ErrPartitionedForeignKeyUnsettled), want: executor.CodePartitionedForeignKeyUnsettled},
		{name: "partition detach pending", err: fmt.Errorf("app.t: %w", executor.ErrPartitionDetachPending), want: executor.CodePartitionDetachPending},
		{name: "partitioned index tablespace", err: fmt.Errorf("app.idx: %w", executor.ErrPartitionedIndexTablespace), want: executor.CodePartitionedIndexTablespace},
		{name: "partitioned index unsettled", err: fmt.Errorf("app.t: %w", executor.ErrPartitionedIndexUnsettled), want: executor.CodePartitionedIndexUnsettled},
		{name: "replica identity drop", err: fmt.Errorf("drop: %w", executor.ErrReplicaIdentityDrop), want: executor.CodeReplicaIdentityDrop},
		{name: "adopted index drift", err: fmt.Errorf("adopt: %w", executor.ErrAdoptedIndexDrift), want: executor.CodeAdoptedIndexDrift},
		{name: "dropped index drift", err: fmt.Errorf("drop: %w", executor.ErrDroppedIndexDrift), want: executor.CodeDroppedIndexDrift},
		{name: "dropped index replaced", err: fmt.Errorf("drop: %w", executor.ErrDroppedIndexReplaced), want: executor.CodeDroppedIndexReplaced},
		{name: "dropped object drift", err: fmt.Errorf("drop: %w", executor.ErrDroppedObjectDrift), want: executor.CodeDroppedObjectDrift},
		{name: "dropped object has descendants", err: fmt.Errorf("drop: %w", executor.ErrDroppedObjectHasDescendants), want: executor.CodeDroppedObjectHasDescendants},
		{name: "dropped table has parent", err: fmt.Errorf("drop: %w", executor.ErrDroppedTableHasParent), want: executor.CodeDroppedTableHasParent},
		{name: "dropped table has dependents", err: fmt.Errorf("drop: %w", executor.ErrDroppedTableHasDependents), want: executor.CodeDroppedTableHasDependents},
		{name: "dropped column partition key", err: fmt.Errorf("drop: %w", executor.ErrDroppedColumnPartitionKey), want: executor.CodeDroppedColumnPartitionKey},
		{name: "dropped column partition dependent", err: fmt.Errorf("drop: %w", executor.ErrDroppedColumnPartitionDependent), want: executor.CodeDroppedColumnPartitionDependent},
		{name: "validated constraint drift", err: fmt.Errorf("validate: %w", executor.ErrValidatedConstraintDrift), want: executor.CodeValidatedConstraintDrift},
		{name: "renamed index drift", err: fmt.Errorf("rename: %w", executor.ErrRenamedIndexDrift), want: executor.CodeRenamedIndexDrift},
		{name: "enum type drift", err: fmt.Errorf("add value: %w", executor.ErrEnumTypeDrift), want: executor.CodeEnumTypeDrift},
		{name: "enum label exists", err: fmt.Errorf("add value: %w", executor.ErrEnumLabelExists), want: executor.CodeEnumLabelExists},
		{name: "enum neighbor missing", err: fmt.Errorf("add value: %w", executor.ErrEnumNeighborMissing), want: executor.CodeEnumNeighborMissing},
		{name: "table not found", err: executor.ErrTableNotFound, want: executor.CodeTableNotFound},
		{name: "invariant violation", err: executor.ErrInvariantViolation, want: executor.CodeInvariantViolation},
		{name: "untyped error is the fallback", err: errors.New("connection reset"), want: executor.CodeExecutionFailed},
		{
			name: "wrapped sentinel still maps",
			err:  fmt.Errorf("sequence step 2 of 3: %w", executor.ErrUnsupportedSequenceStep),
			want: executor.CodeUnsupportedSequenceStep,
		},
		{
			name: "step error carries its cause's code",
			err: &executor.SequenceStepError{
				Step: 2, Total: 3, Kind: executor.StepBrief, SQL: "ALTER TABLE s.t ADD c int",
				Err: &executor.BudgetError{Cause: executor.CauseStatement, Budget: time.Second},
			},
			want: executor.CodeBudgetStatementExceeded,
		},
		{
			name: "step error with an untyped cause is the fallback",
			err: &executor.SequenceStepError{
				Step: 1, Total: 1, Kind: executor.StepBrief, SQL: "ALTER TABLE s.t ADD c int",
				Err: errors.New("server closed the connection"),
			},
			want: executor.CodeExecutionFailed,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, executor.OutcomeCode(tt.err))
		})
	}
}

// TestCodePermanentClassifiesEveryCode pins each code's retry class, and
// pins the case set to Codes() so a code added to the vocabulary has to be
// classified here before it can land: an adapter that reads Permanent()
// for a code nobody decided on would get the unclassified default.
func TestCodePermanentClassifiesEveryCode(t *testing.T) {
	want := map[executor.Code]bool{
		executor.CodeBudgetLockExceeded:                     false,
		executor.CodeBudgetStatementExceeded:                false,
		executor.CodeBlockingOutcomeUnknown:                 false,
		executor.CodeCommitOutcomeUnknown:                   false,
		executor.CodeDropOutcomeUnknown:                     false,
		executor.CodeRowSecurityRefused:                     true,
		executor.CodeRowSecurityPlanChanged:                 true,
		executor.CodeRowSecurityOutcomeUnknown:              false,
		executor.CodeInvalidBlockingBudget:                  true,
		executor.CodeUnsupportedAcceptedBlocking:            true,
		executor.CodeCancelledByCaller:                      false,
		executor.CodeCancelledExternally:                    false,
		executor.CodeInvalidIndexOwnLeftover:                false,
		executor.CodeInvalidIndexBuildInFlight:              false,
		executor.CodeInvalidIndexAbandoned:                  false,
		executor.CodeInvalidIndexOtherTable:                 true,
		executor.CodeInvalidIndexNotDroppable:               true,
		executor.CodeInvalidIndexBuilderUnobservable:        false,
		executor.CodeInvalidIndexUnproven:                   false,
		executor.CodeNotCreateIndex:                         true,
		executor.CodeNotPartitionedTable:                    true,
		executor.CodePartitionedIndexNameTaken:              true,
		executor.CodeForeignTablePartition:                  true,
		executor.CodeUniqueIndexPartitionKey:                true,
		executor.CodeTableLockUnconfirmed:                   true,
		executor.CodePartitionedForeignKeyNameTaken:         true,
		executor.CodePartitionedForeignKeyConflict:          true,
		executor.CodePartitionedForeignKeyReferencedTable:   true,
		executor.CodeNotAddForeignKey:                       true,
		executor.CodePartitionedForeignKeyUnsupportedShape:  true,
		executor.CodePartitionedForeignKeyParentConflict:    true,
		executor.CodePartitionedForeignKeySubPartitioned:    true,
		executor.CodePartitionedForeignKeyNotValidSupported: true,
		executor.CodePartitionedForeignKeyUnsettled:         false,
		executor.CodePartitionDetachPending:                 true,
		executor.CodePartitionedIndexTablespace:             true,
		executor.CodePartitionedIndexUnsettled:              false,
		executor.CodeEmptySequence:                          true,
		executor.CodeUnsupportedSequenceStep:                true,
		executor.CodeUnsupportedPartitionedParent:           true,
		executor.CodeNotConcurrentIndexBuild:                true,
		executor.CodeUnnamedIndex:                           true,
		executor.CodeUnqualifiedTable:                       true,
		executor.CodeIfNotExistsUnsupported:                 true,
		executor.CodeCreateCollision:                        true,
		executor.CodeCreateNameMismatch:                     true,
		executor.CodeCreateNamesUnverified:                  false,
		executor.CodeCreateOwnerMismatch:                    true,
		executor.CodeCreateOwnerUnverified:                  false,
		executor.CodeDuplicateCreateName:                    true,
		executor.CodePartitionOfUnsupported:                 true,
		executor.CodeUnsupportedCreateStep:                  true,
		executor.CodePoolTooSmall:                           true,
		executor.CodeReplicaIdentityDrop:                    true,
		executor.CodeAdoptedIndexDrift:                      true,
		executor.CodeDroppedIndexDrift:                      true,
		executor.CodeDroppedIndexReplaced:                   true,
		executor.CodeDroppedObjectDrift:                     true,
		executor.CodeDroppedObjectHasDescendants:            true,
		executor.CodeDroppedTableHasParent:                  true,
		executor.CodeDroppedTableHasDependents:              true,
		executor.CodeDroppedColumnPartitionKey:              true,
		executor.CodeDroppedColumnPartitionDependent:        true,
		executor.CodeValidatedConstraintDrift:               true,
		executor.CodeRenamedIndexDrift:                      true,
		executor.CodeEnumTypeDrift:                          true,
		executor.CodeEnumLabelExists:                        true,
		executor.CodeEnumNeighborMissing:                    true,
		executor.CodeTableNotFound:                          true,
		executor.CodeDataFault:                              true,
		executor.CodeInvariantViolation:                     true,
		executor.CodeExecutionFailed:                        false,
	}

	classified := make([]executor.Code, 0, len(want))
	for c := range want {
		classified = append(classified, c)
	}
	assert.ElementsMatch(t, executor.Codes(), classified, "every code in Codes() is classified here, and nothing else is")

	for _, c := range executor.Codes() {
		t.Run(string(c), func(t *testing.T) {
			assert.Equal(t, want[c], c.Permanent())
		})
	}
	assert.False(t, executor.Code("").Permanent(), "the empty code (no error) is not permanent")
}

func TestSequenceStepErrorCodeMatchesOutcomeCode(t *testing.T) {
	stepErr := &executor.SequenceStepError{
		Step: 1, Total: 2, Kind: executor.StepConcurrentIndexBuild, SQL: "CREATE INDEX CONCURRENTLY i ON s.t (c)",
		Err: &executor.InvalidIndexError{Schema: "s", Index: "i", Cleanup: executor.ErrBuildLeftInvalidIndex},
	}
	assert.Equal(t, executor.CodeInvalidIndexOwnLeftover, stepErr.Code())
	assert.Equal(t, stepErr.Code(), executor.OutcomeCode(stepErr))
}

// TestSequenceReportJSONContractIsStable pins the report's wire shape:
// consumers parse these exact keys, so a key change is a contract break
// this test makes deliberate.
func TestSequenceReportJSONContractIsStable(t *testing.T) {
	rep := executor.SequenceReport{
		Steps: []executor.StepReport{
			{
				SQL:      "ALTER TABLE s.t ADD CONSTRAINT c CHECK (v > 0) NOT VALID",
				Kind:     executor.StepBrief,
				Duration: 20 * time.Millisecond,
			},
			{
				SQL:      "CREATE INDEX CONCURRENTLY i ON s.t (v)",
				Kind:     executor.StepConcurrentIndexBuild,
				Duration: 1500 * time.Millisecond,
				Index: &executor.IndexBuildReport{
					Schema:        "s",
					Index:         "i",
					IndexOID:      41235,
					Duration:      1400 * time.Millisecond,
					ServerVersion: "16.4",
				},
			},
		},
	}
	got, err := json.Marshal(rep)
	require.NoError(t, err)
	require.JSONEq(t, `{
		"steps": [
			{
				"sql": "ALTER TABLE s.t ADD CONSTRAINT c CHECK (v > 0) NOT VALID",
				"kind": "brief",
				"duration_ns": 20000000
			},
			{
				"sql": "CREATE INDEX CONCURRENTLY i ON s.t (v)",
				"kind": "concurrent-index-build",
				"duration_ns": 1500000000,
				"index": {
					"schema": "s",
					"index": "i",
					"index_oid": 41235,
					"duration_ns": 1400000000,
					"server_version": "16.4"
				}
			}
		]
	}`, string(got))
}

// An invariant breach is the fail-closed outcome, so its code outranks every
// typed failure that carries it.
func TestOutcomeCodeInvariantViolationOutranksTypedFailures(t *testing.T) {
	breach := fmt.Errorf("%w: LK-2", executor.ErrInvariantViolation)
	for name, err := range map[string]error{
		"invalid index":  &executor.InvalidIndexError{Schema: "s", Index: "i", Build: breach, Cleanup: executor.ErrBuildLeftInvalidIndex},
		"budget":         fmt.Errorf("%w: %w", &executor.BudgetError{Cause: executor.CauseStatement, Budget: time.Second}, breach),
		"unknown commit": &executor.CommitOutcomeUnknownError{Err: breach},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, executor.CodeInvariantViolation, executor.OutcomeCode(err))
		})
	}
	invalidErr := &executor.InvalidIndexError{Schema: "s", Index: "i", Build: breach, Cleanup: executor.ErrBuildLeftInvalidIndex}
	assert.Equal(t, executor.CodeInvariantViolation, invalidErr.Code(), "the typed error's own code agrees with OutcomeCode")
}
