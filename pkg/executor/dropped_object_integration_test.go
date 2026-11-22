package executor_test

import (
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil/modeltest"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/schemadiff"
)

func constraintExists(t *testing.T, pool *pgxpool.Pool, schema, table, name string) bool {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT count(*) FROM pg_constraint WHERE conrelid = $1::regclass AND conname = $2`,
		schema+"."+table, name).Scan(&n))
	return n == 1
}

func columnNotNull(t *testing.T, pool *pgxpool.Pool, schema, table, column string) bool {
	t.Helper()
	var notNull bool
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT attnotnull FROM pg_attribute WHERE attrelid = $1::regclass AND attname = $2`,
		schema+"."+table, column).Scan(&notNull))
	return notNull
}

func runDropping(t *testing.T, pool *pgxpool.Pool, schema string, d schemadiff.DroppedObject, sql string) error {
	t.Helper()
	pt := mustPreflight(t, pool, schema, "t")
	_, err := executor.RunSequenceDropping(t.Context(), pool, pt, []string{sql}, runBudget, executor.DefaultRetryPolicy(), d)
	return err
}

// A drop whose definition is the live object's drops it, for a column, a
// constraint, and a NOT NULL alike.
func TestRunSequenceDroppingDropsAnUnchangedObject(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.t (id int PRIMARY KEY, note text DEFAULT 'x', qty int NOT NULL, CONSTRAINT t_qty_check CHECK (qty > 0))`, schema))
	require.NoError(t, err)

	require.NoError(t, runDropping(t, pool, schema, modeltest.PlannedDrop(t, pool, schema, "t", schemadiff.DroppedColumn, "note"),
		fmt.Sprintf("ALTER TABLE %s.t DROP COLUMN note", schema)))
	assert.False(t, columnExists(t, pool, schema, "t", "note"))

	require.NoError(t, runDropping(t, pool, schema, modeltest.PlannedDrop(t, pool, schema, "t", schemadiff.DroppedConstraint, "t_qty_check"),
		fmt.Sprintf("ALTER TABLE %s.t DROP CONSTRAINT t_qty_check", schema)))
	assert.False(t, constraintExists(t, pool, schema, "t", "t_qty_check"))

	require.NoError(t, runDropping(t, pool, schema, modeltest.PlannedDrop(t, pool, schema, "t", schemadiff.DroppedNotNull, "qty"),
		fmt.Sprintf("ALTER TABLE %s.t ALTER COLUMN qty DROP NOT NULL", schema)))
	assert.False(t, columnNotNull(t, pool, schema, "t", "qty"))
}

// A column dropped and added again under its name is a different column:
// it has another attribute number, and the drop the plan reviewed refuses,
// dropping nothing. The same holds for a column whose type, default, or
// collation changed after planning.
func TestRunSequenceDroppingRefusesAColumnThatChanged(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.t (id int PRIMARY KEY, note text, label varchar(20), flag int DEFAULT 1, memo text);
		INSERT INTO %[1]s.t VALUES (1, 'keep', 'lbl', 1)`, schema))
	require.NoError(t, err)
	dropNote := modeltest.PlannedDrop(t, pool, schema, "t", schemadiff.DroppedColumn, "note")
	dropLabel := modeltest.PlannedDrop(t, pool, schema, "t", schemadiff.DroppedColumn, "label")
	dropFlag := modeltest.PlannedDrop(t, pool, schema, "t", schemadiff.DroppedColumn, "flag")
	dropMemo := modeltest.PlannedDrop(t, pool, schema, "t", schemadiff.DroppedColumn, "memo")

	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		ALTER TABLE %[1]s.t DROP COLUMN note;
		ALTER TABLE %[1]s.t ADD COLUMN note text;
		ALTER TABLE %[1]s.t ALTER COLUMN label TYPE varchar(40);
		ALTER TABLE %[1]s.t ALTER COLUMN flag SET DEFAULT 2;
		ALTER TABLE %[1]s.t ALTER COLUMN memo TYPE text COLLATE "C"`, schema))
	require.NoError(t, err)

	for _, tc := range []struct {
		name string
		d    schemadiff.DroppedObject
	}{
		{"dropped and added again", dropNote},
		{"type changed", dropLabel},
		{"default changed", dropFlag},
		{"collation changed", dropMemo},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := runDropping(t, pool, schema, tc.d, fmt.Sprintf("ALTER TABLE %s.t DROP COLUMN %s", schema, tc.d.Name))
			require.ErrorIs(t, err, executor.ErrDroppedObjectDrift)
			assert.Equal(t, executor.CodeDroppedObjectDrift, executor.OutcomeCode(err))
			assert.Contains(t, err.Error(), tc.d.Name)
			assert.True(t, columnExists(t, pool, schema, "t", tc.d.Name), "the column must survive a refused drop")
		})
	}
}

// A constraint replaced under its name, or one that is gone, refuses the
// drop of the constraint the plan reviewed. The replacement survives.
func TestRunSequenceDroppingRefusesAConstraintThatChanged(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.t (id int PRIMARY KEY, qty int, CONSTRAINT t_qty_check CHECK (qty > 0), CONSTRAINT t_gone_check CHECK (qty < 1000))`, schema))
	require.NoError(t, err)
	reused := modeltest.PlannedDrop(t, pool, schema, "t", schemadiff.DroppedConstraint, "t_qty_check")
	gone := modeltest.PlannedDrop(t, pool, schema, "t", schemadiff.DroppedConstraint, "t_gone_check")

	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		ALTER TABLE %[1]s.t DROP CONSTRAINT t_qty_check;
		ALTER TABLE %[1]s.t ADD CONSTRAINT t_qty_check CHECK (qty > 100);
		ALTER TABLE %[1]s.t DROP CONSTRAINT t_gone_check`, schema))
	require.NoError(t, err)

	err = runDropping(t, pool, schema, reused, fmt.Sprintf("ALTER TABLE %s.t DROP CONSTRAINT t_qty_check", schema))
	require.ErrorIs(t, err, executor.ErrDroppedObjectDrift)
	assert.Equal(t, executor.CodeDroppedObjectDrift, executor.OutcomeCode(err))
	assert.True(t, constraintExists(t, pool, schema, "t", "t_qty_check"), "the replacement constraint must survive")

	err = runDropping(t, pool, schema, gone, fmt.Sprintf("ALTER TABLE %s.t DROP CONSTRAINT t_gone_check", schema))
	require.ErrorIs(t, err, executor.ErrDroppedObjectDrift)
	assert.Contains(t, err.Error(), "no constraint named t_gone_check any more")
}

// A NOT NULL that is no longer there, or sits on a column that was dropped
// and added again, refuses the drop of the NOT NULL the plan reviewed.
func TestRunSequenceDroppingRefusesANotNullThatChanged(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.t (id int PRIMARY KEY, a int NOT NULL, b int NOT NULL)`, schema))
	require.NoError(t, err)
	nullable := modeltest.PlannedDrop(t, pool, schema, "t", schemadiff.DroppedNotNull, "a")
	readded := modeltest.PlannedDrop(t, pool, schema, "t", schemadiff.DroppedNotNull, "b")

	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		ALTER TABLE %[1]s.t ALTER COLUMN a DROP NOT NULL;
		ALTER TABLE %[1]s.t DROP COLUMN b;
		ALTER TABLE %[1]s.t ADD COLUMN b int NOT NULL DEFAULT 0`, schema))
	require.NoError(t, err)

	err = runDropping(t, pool, schema, nullable, fmt.Sprintf("ALTER TABLE %s.t ALTER COLUMN a DROP NOT NULL", schema))
	require.ErrorIs(t, err, executor.ErrDroppedObjectDrift)
	err = runDropping(t, pool, schema, readded, fmt.Sprintf("ALTER TABLE %s.t ALTER COLUMN b DROP NOT NULL", schema))
	require.ErrorIs(t, err, executor.ErrDroppedObjectDrift)
	assert.True(t, columnNotNull(t, pool, schema, "t", "b"), "the new column's NOT NULL must survive a refused drop")
}

// A planned drop that no step performs, or that a statement of another
// shape would perform, is a breach of the plan's contract: it is refused
// before anything runs.
func TestRunSequenceDroppingRefusesAStatementThatDoesNotDropTheObject(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`CREATE TABLE %[1]s.t (id int PRIMARY KEY, note text, other text)`, schema))
	require.NoError(t, err)
	d := modeltest.PlannedDrop(t, pool, schema, "t", schemadiff.DroppedColumn, "note")

	for name, sql := range map[string]string{
		"another column":    fmt.Sprintf("ALTER TABLE %s.t DROP COLUMN other", schema),
		"another kind":      fmt.Sprintf("ALTER TABLE %s.t ADD COLUMN extra int", schema),
		"two drops at once": fmt.Sprintf("ALTER TABLE %s.t DROP COLUMN note, DROP COLUMN other", schema),
	} {
		t.Run(name, func(t *testing.T) {
			err := runDropping(t, pool, schema, d, sql)
			require.ErrorIs(t, err, executor.ErrInvariantViolation)
			assert.True(t, columnExists(t, pool, schema, "t", "note"))
			assert.True(t, columnExists(t, pool, schema, "t", "other"))
		})
	}
}

// A column's dependents are part of what the plan reviewed. A drop that
// takes along an index, a constraint of any kind, or a sequence the column
// owns that the plan did not list refuses, dropping nothing, however the
// dependent came to exist after the review.
func TestRunSequenceDroppingRefusesAColumnWithADependentAddedAfterPlanning(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup string
		want  string
		alive func(t *testing.T, pool *pgxpool.Pool, schema string) bool
	}{
		{
			name:  "index",
			setup: `CREATE INDEX t_note_idx ON %[1]s.t (note)`,
			want:  "index t_note_idx",
			alive: func(t *testing.T, pool *pgxpool.Pool, schema string) bool {
				return relationExists(t, pool, schema, "t_note_idx")
			},
		},
		{
			name:  "unique constraint",
			setup: `ALTER TABLE %[1]s.t ADD CONSTRAINT t_note_key UNIQUE (note)`,
			want:  "constraint t_note_key",
			alive: func(t *testing.T, pool *pgxpool.Pool, schema string) bool {
				return constraintExists(t, pool, schema, "t", "t_note_key")
			},
		},
		{
			name:  "check constraint",
			setup: `ALTER TABLE %[1]s.t ADD CONSTRAINT t_note_check CHECK (note <> '')`,
			want:  "constraint t_note_check",
			alive: func(t *testing.T, pool *pgxpool.Pool, schema string) bool {
				return constraintExists(t, pool, schema, "t", "t_note_check")
			},
		},
		{
			name: "foreign key",
			setup: `CREATE UNIQUE INDEX other_key ON %[1]s.other (code);
			        ALTER TABLE %[1]s.t ADD CONSTRAINT t_note_fk FOREIGN KEY (note) REFERENCES %[1]s.other (code)`,
			want: "constraint t_note_fk",
			alive: func(t *testing.T, pool *pgxpool.Pool, schema string) bool {
				return constraintExists(t, pool, schema, "t", "t_note_fk")
			},
		},
		{
			name: "owned sequence",
			setup: `CREATE SEQUENCE %[1]s.t_note_seq;
			        ALTER SEQUENCE %[1]s.t_note_seq OWNED BY %[1]s.t.note`,
			want: "owned sequence",
			alive: func(t *testing.T, pool *pgxpool.Pool, schema string) bool {
				return relationExists(t, pool, schema, "t_note_seq")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, schema := newPool(t)
			_, err := pool.Exec(t.Context(), fmt.Sprintf(`
				CREATE TABLE %[1]s.other (code text);
				CREATE TABLE %[1]s.t (id int PRIMARY KEY, note text)`, schema))
			require.NoError(t, err)
			d := modeltest.PlannedDrop(t, pool, schema, "t", schemadiff.DroppedColumn, "note")
			assert.Empty(t, d.Dependents, "nothing depends on the column when the plan is made")

			_, err = pool.Exec(t.Context(), fmt.Sprintf(tc.setup, schema))
			require.NoError(t, err)

			err = runDropping(t, pool, schema, d, fmt.Sprintf("ALTER TABLE %s.t DROP COLUMN note", schema))
			require.ErrorIs(t, err, executor.ErrDroppedObjectDrift)
			assert.Contains(t, err.Error(), tc.want)
			assert.Contains(t, err.Error(), "did not review")
			assert.True(t, columnExists(t, pool, schema, "t", "note"), "the column must survive a refused drop")
			assert.True(t, tc.alive(t, pool, schema), "the dependent must survive a refused drop")
		})
	}
}

// A dependent the plan listed goes with the column, and one that is gone
// since, removed by an earlier statement of the plan, is no reason to refuse:
// the drop then takes less than the plan said.
func TestRunSequenceDroppingTakesTheDependentsThePlanListed(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.t (id int PRIMARY KEY, note text, tag text);
		CREATE INDEX t_note_idx ON %[1]s.t (note);
		CREATE INDEX t_tag_idx ON %[1]s.t (tag);
		ALTER TABLE %[1]s.t ADD CONSTRAINT t_note_check CHECK (note <> '');
		CREATE SEQUENCE %[1]s.t_note_seq OWNED BY %[1]s.t.note`, schema))
	require.NoError(t, err)

	dropNote := modeltest.PlannedDrop(t, pool, schema, "t", schemadiff.DroppedColumn, "note")
	require.Len(t, dropNote.Dependents, 3)
	assert.Contains(t, dropNote.Dependents[0], "constraint t_note_check")
	assert.Contains(t, dropNote.Dependents[1], "index t_note_idx")
	assert.Contains(t, dropNote.Dependents[2], "owned sequence "+schema+".t_note_seq")
	assert.Contains(t, dropNote.Dependents[2], "START WITH 1 INCREMENT BY 1")
	dropTag := modeltest.PlannedDrop(t, pool, schema, "t", schemadiff.DroppedColumn, "tag")
	require.Len(t, dropTag.Dependents, 1)

	// The plan's own earlier statement removes one of the listed dependents.
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`DROP INDEX %s.t_tag_idx`, schema))
	require.NoError(t, err)
	require.NoError(t, runDropping(t, pool, schema, dropTag, fmt.Sprintf("ALTER TABLE %s.t DROP COLUMN tag", schema)))

	require.NoError(t, runDropping(t, pool, schema, dropNote, fmt.Sprintf("ALTER TABLE %s.t DROP COLUMN note", schema)))
	assert.False(t, columnExists(t, pool, schema, "t", "note"))
	assert.False(t, relationExists(t, pool, schema, "t_note_idx"))
	assert.False(t, relationExists(t, pool, schema, "t_note_seq"))
	assert.False(t, constraintExists(t, pool, schema, "t", "t_note_check"))
}

// An index or constraint replaced under the name the plan listed is another
// object: the listed entry carries its definition, so the drop refuses.
func TestRunSequenceDroppingRefusesAReplacedDependent(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.t (id int PRIMARY KEY, note text);
		CREATE INDEX t_note_idx ON %[1]s.t (note)`, schema))
	require.NoError(t, err)
	d := modeltest.PlannedDrop(t, pool, schema, "t", schemadiff.DroppedColumn, "note")

	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		DROP INDEX %[1]s.t_note_idx;
		CREATE UNIQUE INDEX t_note_idx ON %[1]s.t (note)`, schema))
	require.NoError(t, err)

	err = runDropping(t, pool, schema, d, fmt.Sprintf("ALTER TABLE %s.t DROP COLUMN note", schema))
	require.ErrorIs(t, err, executor.ErrDroppedObjectDrift)
	assert.Contains(t, err.Error(), "UNIQUE")
	assert.True(t, columnExists(t, pool, schema, "t", "note"))
}
