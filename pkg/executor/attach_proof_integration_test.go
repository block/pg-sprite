package executor

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/progress"
)

// These tests run the proof step against the server: it adds each proof
// CHECK NOT VALID and validates it, resumes from whatever an earlier call
// left, and the ATTACH that follows skips the scans the CHECKs prove.

var attachProofTestBudget = ValidateBudget{LockTimeout: 5 * time.Second, Overall: time.Minute}

// eventsWithDefault is a range-partitioned table with a DEFAULT partition and
// a table to attach for January 2027. The DEFAULT partition holds rows of
// other months only, and the new table rows of January only.
const eventsWithDefault = `
	CREATE TABLE %[1]s.events (
		id bigint NOT NULL,
		day date
	) PARTITION BY RANGE (day);
	CREATE TABLE %[1]s.events_default PARTITION OF %[1]s.events DEFAULT;
	CREATE TABLE %[1]s.events_2027_01 (
		id bigint NOT NULL,
		day date
	);
	INSERT INTO %[1]s.events VALUES (1, '2026-12-31'), (2, NULL);
	INSERT INTO %[1]s.events_2027_01 VALUES (3, '2027-01-01'), (4, '2027-01-31')`

const attachJanuary = `ALTER TABLE %[1]s.events
	ATTACH PARTITION %[1]s.events_2027_01
	FOR VALUES FROM ('2027-01-01') TO ('2027-02-01')`

// proofOn resolves table and returns the proof of kind with expression expr
// on it.
func (f attachProofFixture) proofOn(t *testing.T, kind attachProofKind, table, expr string) attachProof {
	t.Helper()
	var oid uint32
	require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT $1::regclass::oid",
		pgx.Identifier{f.schema, table}.Sanitize()).Scan(&oid))
	return attachProof{kind: kind, tableOID: oid, schema: f.schema, table: table, expr: expr}
}

// proofValidated reads whether the proof's CHECK is validated, and whether
// it exists at all.
func (f attachProofFixture) proofValidated(t *testing.T, p attachProof) (validated, exists bool) {
	t.Helper()
	err := f.pool.QueryRow(t.Context(), `SELECT convalidated FROM pg_catalog.pg_constraint
		WHERE conrelid = $1 AND conname = $2 AND contype = 'c'`, p.tableOID, p.name()).Scan(&validated)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false
	}
	require.NoError(t, err)
	return validated, true
}

// From nothing, the step adds each CHECK NOT VALID and validates it, in that
// order, and the ATTACH that follows scans neither table. A second call
// finds both CHECKs and runs nothing.
func TestEnsureAttachProofAddsAndValidatesThenAttachSkipsBothScans(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, eventsWithDefault)
		check, exclusion, err := f.derive(t, attachJanuary)
		require.NoError(t, err)
		bound := f.proofOn(t, attachProofBound, "events_2027_01", check)
		def := f.proofOn(t, attachProofDefault, "events_default", exclusion)

		for _, p := range []attachProof{bound, def} {
			var ran []string
			res, err := ensureAttachProof(t.Context(), f.pool, p, attachProofTestBudget,
				func(_ progress.Operation, sql string) { ran = append(ran, sql) })
			require.NoError(t, err, p.kind)
			assert.Equal(t, attachProofAdded, res.action, p.kind)
			assert.Equal(t, p.name(), res.constraint)
			assert.Equal(t, []string{p.addSQL(), p.validateSQL()}, ran, "add NOT VALID, then validate")
			validated, exists := f.proofValidated(t, p)
			assert.True(t, exists && validated, p.kind)

			ran = nil
			again, err := ensureAttachProof(t.Context(), f.pool, p, attachProofTestBudget,
				func(_ progress.Operation, sql string) { ran = append(ran, sql) })
			require.NoError(t, err)
			assert.Equal(t, attachProofFound, again.action, "a rerun finds the validated CHECK")
			assert.Equal(t, res.constraintOID, again.constraintOID)
			assert.Empty(t, ran, "a rerun runs nothing")
		}

		requireNoScan(t, f.attach(t, attachJanuary), "events_2027_01", "events_default")
	})
}

// A run that stopped after its ADD leaves the CHECK NOT VALID. The next call
// validates that CHECK, under the same OID, and adds nothing.
func TestEnsureAttachProofResumesANotValidCheck(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, eventsWithDefault)
		check, _, err := f.derive(t, attachJanuary)
		require.NoError(t, err)
		p := f.proofOn(t, attachProofBound, "events_2027_01", check)
		_, err = f.pool.Exec(t.Context(), p.addSQL())
		require.NoError(t, err)
		var oid uint32
		require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT oid FROM pg_catalog.pg_constraint
			WHERE conrelid = $1 AND conname = $2`, p.tableOID, p.name()).Scan(&oid))

		var ran []string
		res, err := ensureAttachProof(t.Context(), f.pool, p, attachProofTestBudget,
			func(_ progress.Operation, sql string) { ran = append(ran, sql) })
		require.NoError(t, err)
		assert.Equal(t, attachProofValidated, res.action)
		assert.Equal(t, oid, res.constraintOID, "the CHECK the stopped run added")
		assert.Equal(t, []string{p.validateSQL()}, ran)
		validated, _ := f.proofValidated(t, p)
		assert.True(t, validated)
	})
}

// A row of the new table outside the bound breaks the bound proof, and a row
// of the DEFAULT partition inside the bound breaks the exclusion. Each
// validation fails typed and leaves its CHECK NOT VALID, still enforced for
// new rows; once the row is moved, the next call validates the same CHECK.
func TestEnsureAttachProofFailsTypedOnRowsThatBreakIt(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		t.Run("a row of the new table outside the bound", func(t *testing.T) {
			f := newFixture(t)
			f.exec(t, eventsWithDefault)
			f.exec(t, `INSERT INTO %[1]s.events_2027_01 VALUES (5, '2027-02-01')`)
			check, _, err := f.derive(t, attachJanuary)
			require.NoError(t, err)
			p := f.proofOn(t, attachProofBound, "events_2027_01", check)

			_, err = ensureAttachProof(t.Context(), f.pool, p, attachProofTestBudget, nil)
			require.ErrorIs(t, err, errAttachProofViolation)
			validated, exists := f.proofValidated(t, p)
			assert.True(t, exists, "the CHECK stays")
			assert.False(t, validated, "NOT VALID")
			_, err = f.pool.Exec(t.Context(), "INSERT INTO "+pgx.Identifier{f.schema, "events_2027_01"}.Sanitize()+" VALUES (6, '2027-03-01')")
			require.Error(t, err, "the NOT VALID CHECK refuses a new row outside the bound")

			f.exec(t, `UPDATE %[1]s.events_2027_01 SET day = '2027-01-15' WHERE id = 5`)
			res, err := ensureAttachProof(t.Context(), f.pool, p, attachProofTestBudget, nil)
			require.NoError(t, err)
			assert.Equal(t, attachProofValidated, res.action)
		})

		t.Run("a row of the DEFAULT partition inside the bound", func(t *testing.T) {
			f := newFixture(t)
			f.exec(t, eventsWithDefault)
			f.exec(t, `INSERT INTO %[1]s.events VALUES (7, '2027-01-10')`)
			_, exclusion, err := f.derive(t, attachJanuary)
			require.NoError(t, err)
			p := f.proofOn(t, attachProofDefault, "events_default", exclusion)

			_, err = ensureAttachProof(t.Context(), f.pool, p, attachProofTestBudget, nil)
			require.ErrorIs(t, err, errAttachProofViolation)
			validated, exists := f.proofValidated(t, p)
			assert.True(t, exists)
			assert.False(t, validated)
		})
	})
}

// A constraint that is not a CHECK under the proof's name is refused before
// anything is added, and left as it was.
func TestEnsureAttachProofRefusesANameHeldByAnotherConstraint(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, eventsWithDefault)
		check, _, err := f.derive(t, attachJanuary)
		require.NoError(t, err)
		p := f.proofOn(t, attachProofBound, "events_2027_01", check)
		_, err = f.pool.Exec(t.Context(), "ALTER TABLE "+pgx.Identifier{f.schema, "events_2027_01"}.Sanitize()+
			" ADD CONSTRAINT "+pgx.Identifier{p.name()}.Sanitize()+" UNIQUE (id)")
		require.NoError(t, err)

		_, err = ensureAttachProof(t.Context(), f.pool, p, attachProofTestBudget, nil)
		require.ErrorIs(t, err, errAttachProofNameTaken)
		var contype string
		require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT contype::text FROM pg_catalog.pg_constraint
			WHERE conrelid = $1 AND conname = $2`, p.tableOID, p.name()).Scan(&contype))
		assert.Equal(t, "u", contype, "the unique constraint is left as it was")
	})
}

// A table renamed after the run resolved it is not the table the proof
// names: the step stops before it adds anything.
func TestEnsureAttachProofRefusesATableRenamedSinceItWasResolved(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, eventsWithDefault)
		check, _, err := f.derive(t, attachJanuary)
		require.NoError(t, err)
		p := f.proofOn(t, attachProofBound, "events_2027_01", check)
		f.exec(t, `
			ALTER TABLE %[1]s.events_2027_01 RENAME TO events_2027_01_old;
			CREATE TABLE %[1]s.events_2027_01 (
				id bigint NOT NULL,
				day date
			)`)

		_, err = ensureAttachProof(t.Context(), f.pool, p, attachProofTestBudget, nil)
		require.ErrorIs(t, err, ErrInvariantViolation)
		var n int
		require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT count(*) FROM pg_catalog.pg_constraint
			WHERE conname = $1`, p.name()).Scan(&n))
		assert.Zero(t, n, "nothing was added to either table")
	})
}
