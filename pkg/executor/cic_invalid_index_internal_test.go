package executor

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
)

func TestInvalidIndexErrorAdviceMatchesProof(t *testing.T) {
	tests := []struct {
		name          string
		cleanup       error
		wantsDrop     bool
		wantsRecovery bool
	}{
		{name: "proven own leftover names the drop", cleanup: ErrBuildLeftInvalidIndex, wantsDrop: true, wantsRecovery: true},
		{name: "abandoned entry names the recovery only", cleanup: ErrAbandonedInvalidIndex, wantsRecovery: true},
		{name: "in-flight build does not", cleanup: ErrInvalidIndexBuildInFlight},
		{name: "another table's entry does not", cleanup: ErrInvalidIndexOnOtherTable},
		{name: "an entry the server will not drop concurrently does not", cleanup: ErrInvalidIndexNotDroppable},
		{name: "unobservable builder names the recovery only", cleanup: ErrInvalidIndexBuilderUnobservable, wantsRecovery: true},
		{name: "changed target identity does not", cleanup: ErrTargetIdentityChanged},
		{name: "unproven abandonment does not", cleanup: ErrAbandonmentUnproven},
		{name: "an inspection failure does not", cleanup: errors.New("inspect index s.i: closed pool")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &InvalidIndexError{Schema: "s", Index: "i", Table: "t", BuilderPID: 7, Cleanup: tt.cleanup}
			msg := e.Error()
			if tt.wantsDrop {
				assert.Contains(t, msg, `DROP INDEX CONCURRENTLY "s"."i"`)
			} else {
				assert.NotContains(t, msg, "DROP INDEX")
			}
			if tt.wantsRecovery {
				assert.Contains(t, msg, "RebuildAbandonedIndex")
			} else {
				assert.NotContains(t, msg, "RebuildAbandonedIndex")
			}
			assert.Equal(t, tt.wantsRecovery, e.Recoverable(), "the advice and Recoverable must agree")
		})
	}
}

// TestClassifyInvalidIndexOrdersByProofStrength pins the classifier's
// order: a visible builder outranks the table check, the table check
// outranks droppability, droppability outranks visibility, and only a
// fully observable silence on a droppable entry on the target table is
// the caller's silence verdict.
func TestClassifyInvalidIndexOrdersByProofStrength(t *testing.T) {
	target := indexTarget{tableOID: 100, schema: "s"}
	observable := builderFacts{tracking: true}
	tests := []struct {
		name     string
		existing invalidIndex
		silence  error
		want     error
		wantPID  uint32
	}{
		{
			name:     "visible builder on the target table is in flight",
			existing: invalidIndex{oid: 1, tableOID: 100, table: "t", droppable: true, builder: builderFacts{pid: 42, tracking: true}},
			silence:  ErrAbandonedInvalidIndex,
			want:     ErrInvalidIndexBuildInFlight,
			wantPID:  42,
		},
		{
			name:     "visible builder on another table is still in flight",
			existing: invalidIndex{oid: 1, tableOID: 200, table: "u", droppable: true, builder: builderFacts{pid: 42, hiddenRows: 3}},
			silence:  ErrAbandonedInvalidIndex,
			want:     ErrInvalidIndexBuildInFlight,
			wantPID:  42,
		},
		{
			name:     "another table's entry is refused before droppability is consulted",
			existing: invalidIndex{oid: 1, tableOID: 200, table: "u", droppable: false, builder: builderFacts{hiddenRows: 1}},
			silence:  ErrAbandonedInvalidIndex,
			want:     ErrInvalidIndexOnOtherTable,
		},
		{
			name:     "an entry the server will not drop concurrently is refused before visibility is consulted",
			existing: invalidIndex{oid: 1, tableOID: 100, table: "t", droppable: false, builder: builderFacts{hiddenRows: 1}},
			silence:  ErrAbandonedInvalidIndex,
			want:     ErrInvalidIndexNotDroppable,
		},
		{
			name:     "a hidden command makes the silence unobservable",
			existing: invalidIndex{oid: 1, tableOID: 100, table: "t", droppable: true, builder: builderFacts{hiddenRows: 1, tracking: true}},
			silence:  ErrAbandonedInvalidIndex,
			want:     ErrInvalidIndexBuilderUnobservable,
		},
		{
			name:     "activity tracking off makes the silence unobservable",
			existing: invalidIndex{oid: 1, tableOID: 100, table: "t", droppable: true, builder: builderFacts{tracking: false}},
			silence:  ErrAbandonedInvalidIndex,
			want:     ErrInvalidIndexBuilderUnobservable,
		},
		{
			name:     "observable silence on the target table before a build is abandoned",
			existing: invalidIndex{oid: 1, tableOID: 100, table: "t", droppable: true, builder: observable},
			silence:  ErrAbandonedInvalidIndex,
			want:     ErrAbandonedInvalidIndex,
		},
		{
			name:     "observable silence on the target table after this build's failure is its own leftover",
			existing: invalidIndex{oid: 1, tableOID: 100, table: "t", droppable: true, builder: observable},
			silence:  ErrBuildLeftInvalidIndex,
			want:     ErrBuildLeftInvalidIndex,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := classifyInvalidIndex(tt.existing, target, "i", tt.silence)
			require.ErrorIs(t, e, tt.want)
			assert.Equal(t, "s", e.Schema)
			assert.Equal(t, "i", e.Index)
			assert.Equal(t, tt.existing.table, e.Table)
			assert.Equal(t, tt.wantPID, e.BuilderPID)
		})
	}
}

// TestVerifiedBuildReportFailsClosed covers the success-path
// verification's fail-closed branches, which no admissible statement can
// reach through the public API on current server versions (the one shape
// that leaves an invalid entry on success — the concurrent
// partitioned-parent build — is refused by the server itself): an invalid
// entry under the build's name, and an unreadable catalog. Both must
// surface as *InvalidIndexError, never as a clean report.
func TestDroppableColumnMatchesTheServer(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.t (id int PRIMARY KEY, u int UNIQUE, c int, e int, EXCLUDE USING btree (e WITH =));
		CREATE INDEX idx_plain ON %[1]s.t (c);
		CREATE UNIQUE INDEX idx_referenced ON %[1]s.t (c);
		CREATE TABLE %[1]s.r (id int PRIMARY KEY, t_c int REFERENCES %[1]s.t (c));
		CREATE TABLE %[1]s.p (id int, c int) PARTITION BY RANGE (id);
		CREATE TABLE %[1]s.p1 PARTITION OF %[1]s.p FOR VALUES FROM (0) TO (100);
		CREATE INDEX idx_parent ON ONLY %[1]s.p (c);
		CREATE INDEX idx_partition ON %[1]s.p1 (c);
		ALTER INDEX %[1]s.idx_parent ATTACH PARTITION %[1]s.idx_partition`, schema))
	require.NoError(t, err)

	// The server refuses a partitioned table's index as unsupported, and
	// refuses an index some other object depends on — a constraint's, a
	// foreign key's referenced index, or a partition attached to a parent.
	const (
		sqlstateFeatureNotSupported        = "0A000"
		sqlstateDependentObjectsStillExist = "2BP01"
	)
	tests := []struct {
		name      string
		index     string
		droppable bool
		// refusal is the SQLSTATE the server answers DROP INDEX
		// CONCURRENTLY with when the predicate says not droppable.
		refusal string
	}{
		{name: "plain index", index: "idx_plain", droppable: true},
		{name: "primary key's index", index: "t_pkey", refusal: sqlstateDependentObjectsStillExist},
		{name: "unique constraint's index", index: "t_u_key", refusal: sqlstateDependentObjectsStillExist},
		{name: "exclusion constraint's index", index: "t_e_excl", refusal: sqlstateDependentObjectsStillExist},
		{name: "unique index a foreign key references", index: "idx_referenced", refusal: sqlstateDependentObjectsStillExist},
		{name: "partitioned table's index", index: "idx_parent", refusal: sqlstateFeatureNotSupported},
		{name: "attached index partition", index: "idx_partition", refusal: sqlstateDependentObjectsStillExist},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var droppable bool
			require.NoError(t, pool.QueryRow(t.Context(),
				`SELECT `+droppableColumn+`
				   FROM pg_catalog.pg_class c
				   JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
				  WHERE n.nspname OPERATOR(pg_catalog.=) $1 AND c.relname OPERATOR(pg_catalog.=) $2`,
				schema, tt.index).Scan(&droppable))
			assert.Equal(t, tt.droppable, droppable, "the predicate's verdict")

			_, err := pool.Exec(t.Context(), fmt.Sprintf("DROP INDEX CONCURRENTLY %s.%s", schema, tt.index))
			if tt.droppable {
				require.NoError(t, err, "the server drops what the predicate calls droppable")
				return
			}
			var pgErr *pgconn.PgError
			require.ErrorAs(t, err, &pgErr, "the server refuses what the predicate calls not droppable")
			assert.Equal(t, tt.refusal, pgErr.Code)
		})
	}
}

// A pooler that drops lock_timeout and statement_timeout from the startup
// packet leaves the pool to apply them as statements on each new session
// instead. RESET restores what the startup packet carried, so on such an
// endpoint it restores nothing and the session goes back to the pool with
// both bounds at zero. The release must therefore put the bounds back by
// value: a reused session without them is the unbounded state LK-2 exists
// to prevent.
