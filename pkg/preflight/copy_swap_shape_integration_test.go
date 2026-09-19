package preflight_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
)

// copySwapShapeFixture is a schema and a superuser pool: the superuser is a
// SET-usable member of every role, so the copy-and-swap tier proof is minted
// without provisioning and the tests isolate the shape checks.
type copySwapShapeFixture struct {
	serverURL string
	pool      *pgxpool.Pool
	schema    string
}

func newCopySwapShapeFixture(t *testing.T) copySwapShapeFixture {
	t.Helper()
	serverURL := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: serverURL})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return copySwapShapeFixture{serverURL: serverURL, pool: pool, schema: testutil.NewSchema(t, pool)}
}

// exec runs DDL with %s standing for the fixture schema.
func (f copySwapShapeFixture) exec(t *testing.T, ddl string) {
	t.Helper()
	_, err := f.pool.Exec(t.Context(), fmt.Sprintf(ddl, f.schema))
	require.NoError(t, err)
}

// check mints the tier proof and runs the shape check on table.
func (f copySwapShapeFixture) check(t *testing.T, table string) (preflight.CopySwapTarget, error) {
	t.Helper()
	role, err := preflight.CheckPrivileges(t.Context(), f.pool, f.schema, table, preflight.Requirement{Tier: preflight.TierCopyAndSwap})
	require.NoError(t, err)
	return preflight.CheckCopySwapShape(t.Context(), f.pool, f.schema, table, role)
}

// A plain table with one bigint primary key and DEFAULT replica identity is
// the shape v1 supports; the proof carries the resolved schema, the key
// column and type, and the catalog owner.
func TestCheckCopySwapShapeAcceptsSingleIntegerKey(t *testing.T) {
	f := newCopySwapShapeFixture(t)
	f.exec(t, `
		CREATE TABLE %s.orders (
			id bigint PRIMARY KEY,
			note text
		)`)

	target, err := f.check(t, "orders")
	require.NoError(t, err)
	assert.Equal(t, f.schema, target.Schema())
	assert.Equal(t, "orders", target.Table())
	assert.Equal(t, "id", target.PKColumn())
	assert.Equal(t, preflight.PKBigint, target.PKType())
	assert.NotEmpty(t, target.OwnerRole())
}

// REPLICA IDENTITY FULL is the other supported identity: every decoded
// change carries the whole old row, which is a superset of the key.
func TestCheckCopySwapShapeAcceptsReplicaIdentityFull(t *testing.T) {
	f := newCopySwapShapeFixture(t)
	f.exec(t, `
		CREATE TABLE %s.events (
			id integer PRIMARY KEY,
			payload jsonb
		)`)
	f.exec(t, `ALTER TABLE %s.events REPLICA IDENTITY FULL`)

	target, err := f.check(t, "events")
	require.NoError(t, err)
	assert.Equal(t, preflight.PKInteger, target.PKType())
}

// An UNLOGGED source cannot use a permanent LIKE shadow without changing its
// durability at cutover, so the shape gate refuses it.
func TestCheckCopySwapShapeRefusesUnloggedTable(t *testing.T) {
	f := newCopySwapShapeFixture(t)
	f.exec(t, `
		CREATE UNLOGGED TABLE %s.events (
			id bigint PRIMARY KEY,
			payload jsonb
		)`)

	_, err := f.check(t, "events")
	requireCopySwapCause(t, err, preflight.CopySwapCauseUnlogged)
}

// FORCE ROW LEVEL SECURITY subjects the owner to the table's policies, and
// the copier runs as the owner: its reads of the source would be filtered
// and its writes into the policy-carrying shadow rejected, so the shape gate
// refuses. Enabled-but-not-forced RLS is accepted because the owner bypasses
// it.
func TestCheckCopySwapShapeRefusesForceRowLevelSecurity(t *testing.T) {
	f := newCopySwapShapeFixture(t)
	f.exec(t, `
		CREATE TABLE %s.accounts (
			id bigint PRIMARY KEY,
			balance numeric
		)`)
	f.exec(t, `ALTER TABLE %s.accounts ENABLE ROW LEVEL SECURITY`)
	_, err := f.check(t, "accounts")
	require.NoError(t, err, "enabled row-level security does not bind the owner")

	f.exec(t, `ALTER TABLE %s.accounts FORCE ROW LEVEL SECURITY`)
	_, err = f.check(t, "accounts")
	requireCopySwapCause(t, err, preflight.CopySwapCauseForceRLS)
}

// An unqualified target resolves through search_path and the proof still
// carries the catalog schema, so the shadow's deterministic names never
// depend on the session.
func TestCheckCopySwapShapeResolvesSchemaForUnqualifiedTarget(t *testing.T) {
	f := newCopySwapShapeFixture(t)
	f.exec(t, `
		CREATE TABLE %s.items (
			id smallint PRIMARY KEY
		)`)
	pool := testutil.NewCatalogShadowingPool(t, f.serverURL, f.schema)

	role, err := preflight.CheckPrivileges(t.Context(), pool, "", "items", preflight.Requirement{Tier: preflight.TierCopyAndSwap})
	require.NoError(t, err)
	target, err := preflight.CheckCopySwapShape(t.Context(), pool, "", "items", role)
	require.NoError(t, err)
	assert.Equal(t, f.schema, target.Schema())
	assert.Equal(t, preflight.PKSmallint, target.PKType())
}

func requireCopySwapCause(t *testing.T, err error, want preflight.CopySwapRefusalCause) {
	t.Helper()
	var shapeErr *preflight.UnsupportedCopySwapShapeError
	require.ErrorAs(t, err, &shapeErr)
	assert.Equal(t, want, shapeErr.Cause)
	assert.NotEmpty(t, shapeErr.Detail)
}

// A uuid key is outside the integer family the chunker ranges over.
func TestCheckCopySwapShapeRefusesNonIntegerKey(t *testing.T) {
	f := newCopySwapShapeFixture(t)
	f.exec(t, `
		CREATE TABLE %s.sessions (
			id uuid PRIMARY KEY
		)`)

	_, err := f.check(t, "sessions")
	requireCopySwapCause(t, err, preflight.CopySwapCausePKUnsupported)
}

// A composite key has no single chunk column; a table without a primary
// key has none at all. Both report the key-column count that decided it.
func TestCheckCopySwapShapeRefusesCompositeAndMissingKey(t *testing.T) {
	f := newCopySwapShapeFixture(t)
	f.exec(t, `
		CREATE TABLE %s.memberships (
			user_id bigint,
			group_id bigint,
			PRIMARY KEY (user_id, group_id)
		)`)
	f.exec(t, `
		CREATE TABLE %s.heap (
			id bigint,
			note text
		)`)

	_, err := f.check(t, "memberships")
	requireCopySwapCause(t, err, preflight.CopySwapCausePKUnsupported)
	_, err = f.check(t, "heap")
	requireCopySwapCause(t, err, preflight.CopySwapCausePKUnsupported)
}

// REPLICA IDENTITY NOTHING strips the key from decoded UPDATE and DELETE
// events; a named index is outside v1 even when it is the key's own index.
func TestCheckCopySwapShapeRefusesNothingAndIndexReplicaIdentity(t *testing.T) {
	f := newCopySwapShapeFixture(t)
	f.exec(t, `
		CREATE TABLE %s.audit (
			id bigint PRIMARY KEY
		)`)
	f.exec(t, `ALTER TABLE %s.audit REPLICA IDENTITY NOTHING`)
	f.exec(t, `
		CREATE TABLE %s.keyed (
			id bigint PRIMARY KEY,
			code text NOT NULL
		)`)
	f.exec(t, `CREATE UNIQUE INDEX keyed_code_idx ON %s.keyed (code)`)
	f.exec(t, `ALTER TABLE %s.keyed REPLICA IDENTITY USING INDEX keyed_code_idx`)

	_, err := f.check(t, "audit")
	requireCopySwapCause(t, err, preflight.CopySwapCauseReplicaIdentity)
	_, err = f.check(t, "keyed")
	requireCopySwapCause(t, err, preflight.CopySwapCauseReplicaIdentity)
}

// A foreign key in either direction is bound to the table's OID and would
// follow the retained old table through the rename swap.
func TestCheckCopySwapShapeRefusesForeignKeysInEitherDirection(t *testing.T) {
	f := newCopySwapShapeFixture(t)
	f.exec(t, `
		CREATE TABLE %s.parents (
			id bigint PRIMARY KEY
		)`)
	f.exec(t, `
		CREATE TABLE %s.children (
			id bigint PRIMARY KEY,
			parent_id bigint REFERENCES %[1]s.parents (id)
		)`)

	_, err := f.check(t, "parents")
	requireCopySwapCause(t, err, preflight.CopySwapCauseForeignKeys)
	_, err = f.check(t, "children")
	requireCopySwapCause(t, err, preflight.CopySwapCauseForeignKeys)
}

// A user trigger and a rewrite rule are OID-bound dependents; the
// foreign-key-installed internal triggers are not counted here because the
// foreign-key cause owns them.
func TestCheckCopySwapShapeRefusesTriggersAndRules(t *testing.T) {
	f := newCopySwapShapeFixture(t)
	f.exec(t, `
		CREATE TABLE %s.triggered (
			id bigint PRIMARY KEY,
			updated_at timestamptz
		)`)
	f.exec(t, `
		CREATE FUNCTION %s.touch() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			NEW.updated_at := now();
			RETURN NEW;
		END $$`)
	f.exec(t, `CREATE TRIGGER touch_row BEFORE UPDATE ON %s.triggered FOR EACH ROW EXECUTE FUNCTION %[1]s.touch()`)
	f.exec(t, `
		CREATE TABLE %s.ruled (
			id bigint PRIMARY KEY
		)`)
	f.exec(t, `CREATE RULE no_delete AS ON DELETE TO %s.ruled DO INSTEAD NOTHING`)

	_, err := f.check(t, "triggered")
	requireCopySwapCause(t, err, preflight.CopySwapCauseTriggers)
	_, err = f.check(t, "ruled")
	requireCopySwapCause(t, err, preflight.CopySwapCauseTriggers)
}

// A partitioned parent, one of its partitions, and both ends of a classic
// inheritance tree are all refused as partitioned.
func TestCheckCopySwapShapeRefusesPartitioningAndInheritance(t *testing.T) {
	f := newCopySwapShapeFixture(t)
	f.exec(t, `
		CREATE TABLE %s.measurements (
			id bigint NOT NULL,
			taken_at date NOT NULL,
			PRIMARY KEY (id, taken_at)
		) PARTITION BY RANGE (taken_at)`)
	f.exec(t, `CREATE TABLE %s.measurements_2026 PARTITION OF %[1]s.measurements FOR VALUES FROM ('2026-01-01') TO ('2027-01-01')`)
	f.exec(t, `
		CREATE TABLE %s.base (
			id bigint PRIMARY KEY
		)`)
	f.exec(t, `
		CREATE TABLE %s.derived (
			extra text
		) INHERITS (%[1]s.base)`)

	for _, table := range []string{"measurements", "measurements_2026", "base"} {
		_, err := f.check(t, table)
		requireCopySwapCause(t, err, preflight.CopySwapCausePartitioned)
	}
	// The inheritance child has no primary key of its own, but the tree
	// membership is decided first so the refusal names the real cause.
	_, err := f.check(t, "derived")
	requireCopySwapCause(t, err, preflight.CopySwapCausePartitioned)
}

// A proof verified below the copy-and-swap tier cannot mint a target: the
// owner it carries was never proven SET-usable, so shadow objects could be
// created as the wrong role.
func TestCheckCopySwapShapeRejectsLowerTierProof(t *testing.T) {
	f := newCopySwapShapeFixture(t)
	f.exec(t, `
		CREATE TABLE %s.plain (
			id bigint PRIMARY KEY
		)`)
	role, err := preflight.CheckPrivileges(t.Context(), f.pool, f.schema, "plain", preflight.Requirement{Tier: preflight.TierAlterInPlace})
	require.NoError(t, err)

	_, err = preflight.CheckCopySwapShape(t.Context(), f.pool, f.schema, "plain", role)
	require.ErrorIs(t, err, preflight.ErrCopySwapProofMismatch)
	_, err = preflight.CheckCopySwapShape(t.Context(), f.pool, f.schema, "plain", preflight.PrivilegedRole{})
	require.ErrorIs(t, err, preflight.ErrCopySwapProofMismatch)
}

// A missing table is the same typed not-found the other preflight checks
// report, not a shape refusal.
func TestCheckCopySwapShapeReportsMissingTable(t *testing.T) {
	f := newCopySwapShapeFixture(t)
	f.exec(t, `
		CREATE TABLE %s.present (
			id bigint PRIMARY KEY
		)`)
	role, err := preflight.CheckPrivileges(t.Context(), f.pool, f.schema, "present", preflight.Requirement{Tier: preflight.TierCopyAndSwap})
	require.NoError(t, err)

	_, err = preflight.CheckCopySwapShape(t.Context(), f.pool, f.schema, "absent", role)
	require.ErrorIs(t, err, preflight.ErrTableNotFound)
	var shapeErr *preflight.UnsupportedCopySwapShapeError
	assert.False(t, errors.As(err, &shapeErr))
}
