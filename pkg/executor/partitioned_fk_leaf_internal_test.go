package executor

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
)

// leafFKBudget bounds each VALIDATE in these tests.
var leafFKBudget = ValidateBudget{LockTimeout: 5 * time.Second, Overall: time.Minute}

// fkFixture is one subtest's view of a shared server: the engine's bounded
// pool and a schema of its own.
type fkFixture struct {
	pool   *pgxpool.Pool
	schema string
}

// withFKServer runs the subtests against one server, each in a schema of
// its own, so the container start is paid once per test.
func withFKServer(t *testing.T, run func(t *testing.T, newFixture func(t *testing.T) fkFixture)) {
	t.Helper()
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	run(t, func(t *testing.T) fkFixture {
		t.Helper()
		return fkFixture{pool: pool, schema: testutil.NewSchema(t, pool)}
	})
}

// exec runs sql with every %[1]s replaced by the fixture's schema.
func (f fkFixture) exec(t *testing.T, sql string) {
	t.Helper()
	_, err := f.pool.Exec(t.Context(), fmt.Sprintf(sql, f.schema))
	require.NoError(t, err)
}

// events creates the tables every test starts from: accounts, and events,
// range-partitioned into three monthly leaves whose rows each reference an
// account.
func (f fkFixture) events(t *testing.T) {
	t.Helper()
	f.exec(t, `
		CREATE TABLE %[1]s.accounts (
		    id bigint PRIMARY KEY
		)`)
	f.exec(t, `INSERT INTO %[1]s.accounts (id) SELECT g FROM generate_series(1, 100) g`)
	f.exec(t, `
		CREATE TABLE %[1]s.events (
		    id         bigint      NOT NULL,
		    account_id bigint,
		    created_at timestamptz NOT NULL
		) PARTITION BY RANGE (created_at)`)
	f.exec(t, `
		CREATE TABLE %[1]s.events_2026_01 PARTITION OF %[1]s.events
		    FOR VALUES FROM ('2026-01-01') TO ('2026-02-01')`)
	f.exec(t, `
		CREATE TABLE %[1]s.events_2026_02 PARTITION OF %[1]s.events
		    FOR VALUES FROM ('2026-02-01') TO ('2026-03-01')`)
	f.exec(t, `
		CREATE TABLE %[1]s.events_2026_03 PARTITION OF %[1]s.events
		    FOR VALUES FROM ('2026-03-01') TO ('2026-04-01')`)
	f.exec(t, `
		INSERT INTO %[1]s.events (id, account_id, created_at)
		SELECT g, 1 + mod(g, 100), '2026-01-01'::timestamptz + mod(g, 89) * interval '1 day'
		  FROM generate_series(1, 3000) g`)
}

// accountFK is ALTER TABLE events ADD CONSTRAINT events_account_fk FOREIGN
// KEY (account_id) REFERENCES accounts (id), every rule at its default.
func (f fkFixture) accountFK() partitionedFKSpec {
	return partitionedFKSpec{
		name: "events_account_fk", columns: []string{"account_id"},
		refSchema: f.schema, refTable: "accounts", refColumns: []string{"id"},
		match: "s", onUpdate: "a", onDelete: "a",
	}
}

// resolve resolves the spec's referenced table the way the build does.
func (f fkFixture) resolve(t *testing.T, spec partitionedFKSpec) resolvedFK {
	t.Helper()
	major, err := dbconn.ServerMajor(t.Context(), f.pool)
	require.NoError(t, err)
	fk := resolvedFK{spec: spec, serverMajor: major}
	err = briefTx{pool: f.pool}.read(t.Context(), "resolve", func(ctx context.Context, q querier) error {
		fk.ref, err = resolveFKReference(ctx, q, spec)
		return err
	})
	require.NoError(t, err)
	return fk
}

func (f fkFixture) oid(t *testing.T, table string) uint32 {
	t.Helper()
	var oid uint32
	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT $1::regclass::oid`,
		pgx.Identifier{f.schema, table}.Sanitize()).Scan(&oid))
	return oid
}

// leaf returns the partition as the tree listing reads it, the input the
// step takes.
func (f fkFixture) leaf(t *testing.T, name string) partitionNode {
	t.Helper()
	tree, err := listPartitionTree(t.Context(), f.pool, f.oid(t, "events"))
	require.NoError(t, err)
	for _, n := range tree {
		if n.name == name {
			return n
		}
	}
	require.FailNow(t, "no partition "+name)
	return partitionNode{}
}

// fkRow is one foreign key of a table as the tests read it back.
type fkRow struct {
	oid       uint32
	def       string
	validated bool
	parentOID uint32
}

// foreignKeys reads every foreign key of the table, by name, with the
// definition the server prints for it.
func (f fkFixture) foreignKeys(t *testing.T, table string) map[string]fkRow {
	t.Helper()
	rows, err := f.pool.Query(t.Context(), `
		SELECT conname, oid, pg_get_constraintdef(oid), convalidated, conparentid
		  FROM pg_constraint
		 WHERE conrelid = $1 AND contype = 'f'`, f.oid(t, table))
	require.NoError(t, err)
	defer rows.Close()
	out := map[string]fkRow{}
	for rows.Next() {
		var name string
		var r fkRow
		require.NoError(t, rows.Scan(&name, &r.oid, &r.def, &r.validated, &r.parentOID))
		out[name] = r
	}
	require.NoError(t, rows.Err())
	return out
}

func (f fkFixture) ensure(t *testing.T, spec partitionedFKSpec, leaf string) (leafFKResult, error) {
	t.Helper()
	return ensureLeafForeignKey(t.Context(), f.pool, f.resolve(t, spec), f.leaf(t, leaf), leafFKBudget, nil)
}

// requireLeafFK asserts the leaf has the foreign key the step reported,
// validated, adopted by nothing, with the requested definition.
func (f fkFixture) requireLeafFK(t *testing.T, leaf string, res leafFKResult) {
	t.Helper()
	got, ok := f.foreignKeys(t, leaf)[res.constraint]
	require.True(t, ok, "partition %s has foreign key %s", leaf, res.constraint)
	assert.Equal(t, fkRow{
		oid:       res.constraintOID,
		def:       fmt.Sprintf("FOREIGN KEY (account_id) REFERENCES %s.accounts(id)", f.schema),
		validated: true,
	}, got)
}

// A leaf without the foreign key gets it NOT VALID under the derived name and
// then validated; the same call again finds it and changes nothing.
func TestEnsureLeafForeignKeyAddsThenValidates(t *testing.T) {
	withFKServer(t, func(t *testing.T, newFixture func(t *testing.T) fkFixture) {
		f := newFixture(t)
		f.events(t)

		res, err := f.ensure(t, f.accountFK(), "events_2026_01")

		require.NoError(t, err)
		assert.Equal(t, leafFKAdded, res.action)
		assert.Equal(t, derivedPartitionIndexName("events_account_fk", f.leaf(t, "events_2026_01").oid), res.constraint)
		f.requireLeafFK(t, "events_2026_01", res)
		assert.Empty(t, f.foreignKeys(t, "events_2026_02"), "the step touches only its leaf")

		again, err := f.ensure(t, f.accountFK(), "events_2026_01")

		require.NoError(t, err)
		assert.Equal(t, leafFKResult{action: leafFKFound, constraint: res.constraint, constraintOID: res.constraintOID}, again)
		assert.Len(t, f.foreignKeys(t, "events_2026_01"), 1)
	})
}

// The step resumes from what an interrupted run or an operator left: a
// matching foreign key under any name is reused, validated first when it is
// NOT VALID, and never duplicated.
func TestEnsureLeafForeignKeyResumes(t *testing.T) {
	withFKServer(t, func(t *testing.T, newFixture func(t *testing.T) fkFixture) {
		// An interrupted run's NOT VALID foreign key, or an operator's, is
		// validated where it stands, under its own name and OID.
		t.Run("a NOT VALID foreign key is validated", func(t *testing.T) {
			f := newFixture(t)
			f.events(t)
			f.exec(t, `
				ALTER TABLE %[1]s.events_2026_02
				    ADD CONSTRAINT feb_account_fk FOREIGN KEY (account_id) REFERENCES %[1]s.accounts (id)
				    NOT VALID`)
			before := f.foreignKeys(t, "events_2026_02")["feb_account_fk"]

			res, err := f.ensure(t, f.accountFK(), "events_2026_02")

			require.NoError(t, err)
			assert.Equal(t, leafFKResult{action: leafFKValidated, constraint: "feb_account_fk", constraintOID: before.oid}, res)
			f.requireLeafFK(t, "events_2026_02", res)
			assert.Len(t, f.foreignKeys(t, "events_2026_02"), 1)
		})

		t.Run("an operator's validated foreign key is found", func(t *testing.T) {
			f := newFixture(t)
			f.events(t)
			f.exec(t, `
				ALTER TABLE %[1]s.events_2026_02
				    ADD CONSTRAINT feb_account_fk FOREIGN KEY (account_id) REFERENCES %[1]s.accounts (id)`)
			before := f.foreignKeys(t, "events_2026_02")["feb_account_fk"]

			res, err := f.ensure(t, f.accountFK(), "events_2026_02")

			require.NoError(t, err)
			assert.Equal(t, leafFKResult{action: leafFKFound, constraint: "feb_account_fk", constraintOID: before.oid}, res)
			assert.Len(t, f.foreignKeys(t, "events_2026_02"), 1)
		})

		// A foreign key with another ON DELETE is not one the parent would
		// adopt, so the step adds the requested one beside it and leaves the
		// operator's alone.
		t.Run("a foreign key of another definition is left alone", func(t *testing.T) {
			f := newFixture(t)
			f.events(t)
			f.exec(t, `
				ALTER TABLE %[1]s.events_2026_02
				    ADD CONSTRAINT feb_cascade_fk FOREIGN KEY (account_id) REFERENCES %[1]s.accounts (id)
				    ON DELETE CASCADE`)

			res, err := f.ensure(t, f.accountFK(), "events_2026_02")

			require.NoError(t, err)
			assert.Equal(t, leafFKAdded, res.action)
			f.requireLeafFK(t, "events_2026_02", res)
			assert.Len(t, f.foreignKeys(t, "events_2026_02"), 2)
		})

		// From version 18 a foreign key can be NOT ENFORCED, which checks
		// nothing; it is not the requested one, so the step adds an enforced
		// one beside it.
		t.Run("a NOT ENFORCED foreign key is left alone", func(t *testing.T) {
			f := newFixture(t)
			major, err := dbconn.ServerMajor(t.Context(), f.pool)
			require.NoError(t, err)
			if major < 18 {
				t.Skipf("NOT ENFORCED foreign keys arrive in version 18; the server is %d", major)
			}
			f.events(t)
			f.exec(t, `
				ALTER TABLE %[1]s.events_2026_02
				    ADD CONSTRAINT feb_unenforced_fk FOREIGN KEY (account_id) REFERENCES %[1]s.accounts (id)
				    NOT ENFORCED`)

			res, err := f.ensure(t, f.accountFK(), "events_2026_02")

			require.NoError(t, err)
			assert.Equal(t, leafFKAdded, res.action)
			f.requireLeafFK(t, "events_2026_02", res)
			assert.Len(t, f.foreignKeys(t, "events_2026_02"), 2)
		})
	})
}

// VALIDATE takes SHARE UPDATE EXCLUSIVE, which does not wait for a writer's
// ROW EXCLUSIVE: a transaction that has written to the leaf and stays open
// does not hold up the validation.
func TestEnsureLeafForeignKeyValidatesUnderAnOpenWriter(t *testing.T) {
	withFKServer(t, func(t *testing.T, newFixture func(t *testing.T) fkFixture) {
		f := newFixture(t)
		f.events(t)
		f.exec(t, `
			ALTER TABLE %[1]s.events_2026_01
			    ADD CONSTRAINT jan_account_fk FOREIGN KEY (account_id) REFERENCES %[1]s.accounts (id)
			    NOT VALID`)
		writer, err := f.pool.Begin(t.Context())
		require.NoError(t, err)
		defer func() {
			if err := writer.Rollback(context.WithoutCancel(t.Context())); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
				assert.NoError(t, err)
			}
		}()
		_, err = writer.Exec(t.Context(), fmt.Sprintf(
			`INSERT INTO %s.events_2026_01 (id, account_id, created_at) VALUES (100000, 1, '2026-01-02')`, f.schema))
		require.NoError(t, err)

		res, err := f.ensure(t, f.accountFK(), "events_2026_01")

		require.NoError(t, err)
		assert.Equal(t, leafFKValidated, res.action)
		f.requireLeafFK(t, "events_2026_01", res)
		require.NoError(t, writer.Commit(t.Context()))
	})
}

// Every refusal is typed and leaves the leaf's constraints as they were.
func TestEnsureLeafForeignKeyRefusals(t *testing.T) {
	withFKServer(t, func(t *testing.T, newFixture func(t *testing.T) fkFixture) {
		t.Run("the derived name held by another constraint", func(t *testing.T) {
			f := newFixture(t)
			f.events(t)
			name := derivedPartitionIndexName("events_account_fk", f.leaf(t, "events_2026_01").oid)
			f.exec(t, `ALTER TABLE %[1]s.events_2026_01 ADD CONSTRAINT `+pgx.Identifier{name}.Sanitize()+` CHECK (id > 0)`)

			_, err := f.ensure(t, f.accountFK(), "events_2026_01")

			require.ErrorIs(t, err, ErrPartitionedForeignKeyNameTaken)
			assert.Empty(t, f.foreignKeys(t, "events_2026_01"))
		})

		// The parent's statement would adopt one of the two and leave the
		// other behind, so which one becomes the parent's is not left to
		// the server.
		t.Run("two foreign keys of the requested definition", func(t *testing.T) {
			f := newFixture(t)
			f.events(t)
			f.exec(t, `
				ALTER TABLE %[1]s.events_2026_01
				    ADD CONSTRAINT jan_account_fk FOREIGN KEY (account_id) REFERENCES %[1]s.accounts (id)`)
			f.exec(t, `
				ALTER TABLE %[1]s.events_2026_01
				    ADD CONSTRAINT jan_account_fk_2 FOREIGN KEY (account_id) REFERENCES %[1]s.accounts (id)
				    NOT VALID`)
			before := f.foreignKeys(t, "events_2026_01")

			_, err := f.ensure(t, f.accountFK(), "events_2026_01")

			require.ErrorIs(t, err, ErrPartitionedForeignKeyConflict)
			assert.Equal(t, before, f.foreignKeys(t, "events_2026_01"), "nothing was validated or added")
		})

		// A parent's foreign key added the ordinary way already adopted the
		// leaf's; it is not the step's to hand to another parent constraint.
		t.Run("a matching foreign key already adopted by a parent's", func(t *testing.T) {
			f := newFixture(t)
			f.events(t)
			f.exec(t, `
				ALTER TABLE %[1]s.events
				    ADD CONSTRAINT events_account_other_fk FOREIGN KEY (account_id) REFERENCES %[1]s.accounts (id)`)
			before := f.foreignKeys(t, "events_2026_01")

			_, err := f.ensure(t, f.accountFK(), "events_2026_01")

			require.ErrorIs(t, err, ErrPartitionedForeignKeyConflict)
			assert.Equal(t, before, f.foreignKeys(t, "events_2026_01"))
		})

		// The step is written for a leaf; a sub-partitioned node is the
		// caller's to recurse into, not the step's to act on.
		t.Run("a node that is not a leaf", func(t *testing.T) {
			f := newFixture(t)
			f.events(t)
			node := f.leaf(t, "events_2026_01")
			node.relkind = "p"

			_, err := ensureLeafForeignKey(t.Context(), f.pool, f.resolve(t, f.accountFK()), node, leafFKBudget, nil)

			require.ErrorIs(t, err, ErrInvariantViolation)
			assert.Empty(t, f.foreignKeys(t, "events_2026_01"))
		})

		t.Run("a partitioned referenced table", func(t *testing.T) {
			f := newFixture(t)
			f.events(t)
			spec := f.accountFK()
			spec.refTable = "events"

			err := briefTx{pool: f.pool}.read(t.Context(), "resolve", func(ctx context.Context, q querier) error {
				_, err := resolveFKReference(ctx, q, spec)
				return err
			})

			require.ErrorIs(t, err, ErrPartitionedForeignKeyReferencedTable)
		})
	})
}

// A row whose account does not exist fails the VALIDATE, typed as a
// violation with the server's error kept in the chain. The NOT VALID
// foreign key stays, still enforced for new rows, and a rerun once the row
// is fixed validates it where it stands.
func TestEnsureLeafForeignKeyValidateFailureResumes(t *testing.T) {
	withFKServer(t, func(t *testing.T, newFixture func(t *testing.T) fkFixture) {
		f := newFixture(t)
		f.events(t)
		f.exec(t, `INSERT INTO %[1]s.events (id, account_id, created_at) VALUES (90000, 4242, '2026-01-03')`)

		_, err := f.ensure(t, f.accountFK(), "events_2026_01")

		require.ErrorIs(t, err, ErrPartitionedForeignKeyViolation)
		pgErr, ok := errors.AsType[*pgconn.PgError](err)
		require.True(t, ok, "the VALIDATE's own error: %v", err)
		assert.Equal(t, "23503", pgErr.Code)
		name := derivedPartitionIndexName("events_account_fk", f.leaf(t, "events_2026_01").oid)
		left := f.foreignKeys(t, "events_2026_01")[name]
		assert.False(t, left.validated, "the NOT VALID foreign key stays")

		f.exec(t, `DELETE FROM %[1]s.events WHERE id = 90000`)
		res, err := f.ensure(t, f.accountFK(), "events_2026_01")

		require.NoError(t, err)
		assert.Equal(t, leafFKResult{action: leafFKValidated, constraint: name, constraintOID: left.oid}, res)
	})
}
