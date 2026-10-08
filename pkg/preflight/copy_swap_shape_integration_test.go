package preflight_test

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
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
func (f copySwapShapeFixture) check(t *testing.T, table string) (preflight.CopySwapShape, error) {
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

// A DEFERRABLE primary key is checked at the end of each statement, so one
// statement can move a row onto a key another row still holds and the decoded
// stream no longer has one row per key, which the applier's buffer relies on.
// The server also will not use a deferrable key as the DEFAULT replica
// identity, so every UPDATE on the published table would fail. Both the FULL
// and the DEFAULT identity forms are refused, whichever way the constraint is
// initially timed.
func TestCheckCopySwapShapeRefusesDeferrablePrimaryKey(t *testing.T) {
	f := newCopySwapShapeFixture(t)
	f.exec(t, `
		CREATE TABLE %s.slots_full (
			id bigint PRIMARY KEY DEFERRABLE INITIALLY IMMEDIATE,
			label text
		)`)
	f.exec(t, `ALTER TABLE %s.slots_full REPLICA IDENTITY FULL`)
	f.exec(t, `
		CREATE TABLE %s.slots_default (
			id bigint PRIMARY KEY DEFERRABLE INITIALLY DEFERRED,
			label text
		)`)

	_, err := f.check(t, "slots_full")
	requireCopySwapCause(t, err, preflight.CopySwapCausePKUnsupported)
	_, err = f.check(t, "slots_default")
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

// A view or materialized view selecting from the table is an OID-bound
// dependent: its rewrite rule would keep reading the retained old table
// after the swap. A view over an unrelated table does not count against it.
func TestCheckCopySwapShapeRefusesDependentViews(t *testing.T) {
	f := newCopySwapShapeFixture(t)
	f.exec(t, `
		CREATE TABLE %s.viewed (
			id bigint PRIMARY KEY,
			amount numeric
		)`)
	f.exec(t, `CREATE VIEW %s.viewed_totals AS SELECT sum(amount) AS total FROM %[1]s.viewed`)
	f.exec(t, `
		CREATE TABLE %s.materialized (
			id bigint PRIMARY KEY,
			amount numeric
		)`)
	f.exec(t, `CREATE MATERIALIZED VIEW %s.materialized_totals AS SELECT sum(amount) AS total FROM %[1]s.materialized`)
	f.exec(t, `
		CREATE TABLE %s.unviewed (
			id bigint PRIMARY KEY
		)`)

	_, err := f.check(t, "viewed")
	requireCopySwapCause(t, err, preflight.CopySwapCauseDependentViews)
	_, err = f.check(t, "materialized")
	requireCopySwapCause(t, err, preflight.CopySwapCauseDependentViews)
	_, err = f.check(t, "unviewed")
	require.NoError(t, err, "a view over another table is not this table's dependent")
}

// Any publication other than the engine's own for the table is a dependent:
// explicit membership is bound to the table's OID, so subscribers would keep
// following the old table, and a FOR ALL TABLES publication would publish
// the shadow's copy writes. Only the publication wearing the exact name the
// route derives for the table is set aside; a publication that merely wears
// the engine prefix is somebody else's. The test runs in a database of its
// own: a FOR ALL TABLES publication makes every UPDATE and DELETE in its
// database demand a replica identity, which must not reach tables other
// tests share.
func TestCheckCopySwapShapeRefusesPublicationsOtherThanTheEngineOwn(t *testing.T) {
	f := newCopySwapShapeFixtureInOwnDatabase(t)
	f.exec(t, `
		CREATE TABLE %s.published (
			id bigint PRIMARY KEY
		)`)
	f.exec(t, `
		CREATE TABLE %s.engine_published (
			id bigint PRIMARY KEY
		)`)
	f.exec(t, `
		CREATE TABLE %s.prefix_published (
			id bigint PRIMARY KEY
		)`)
	f.exec(t, `
		CREATE TABLE %s.unpublished (
			id bigint PRIMARY KEY
		)`)
	f.exec(t, `CREATE PUBLICATION pub_explicit FOR TABLE %s.published`)
	f.exec(t, `CREATE PUBLICATION pgsprite_0badf00d FOR TABLE %s.prefix_published`)
	f.exec(t, `CREATE PUBLICATION `+preflight.CopySwapDecodingName(f.database(t), f.schema, "engine_published")+` FOR TABLE %s.engine_published`)

	_, err := f.check(t, "published")
	requireCopySwapCause(t, err, preflight.CopySwapCausePublicationMember)
	_, err = f.check(t, "prefix_published")
	requireCopySwapCause(t, err, preflight.CopySwapCausePublicationMember)
	_, err = f.check(t, "engine_published")
	require.NoError(t, err, "the engine's own publication for the table is not a dependent")
	_, err = f.check(t, "unpublished")
	require.NoError(t, err)

	_, err = f.pool.Exec(t.Context(), `CREATE PUBLICATION pub_all FOR ALL TABLES`)
	require.NoError(t, err)
	_, err = f.check(t, "unpublished")
	requireCopySwapCause(t, err, preflight.CopySwapCausePublicationMember)
	_, err = f.check(t, "engine_published")
	requireCopySwapCause(t, err, preflight.CopySwapCausePublicationMember)
}

// A FOR TABLES IN SCHEMA publication publishes every table in its schema,
// the shadow included, so a table in that schema is refused while a table
// in another schema is not. The form exists from PostgreSQL 15.
func TestCheckCopySwapShapeRefusesSchemaPublication(t *testing.T) {
	if major(t) < 15 {
		t.Skip("FOR TABLES IN SCHEMA publications exist from PostgreSQL 15")
	}
	f := newCopySwapShapeFixtureInOwnDatabase(t)
	f.exec(t, `
		CREATE TABLE %s.in_schema (
			id bigint PRIMARY KEY
		)`)
	f.exec(t, `CREATE PUBLICATION pub_schema FOR TABLES IN SCHEMA %s`)
	other := copySwapShapeFixture{serverURL: f.serverURL, pool: f.pool, schema: testutil.NewSchema(t, f.pool)}
	other.exec(t, `
		CREATE TABLE %s.elsewhere (
			id bigint PRIMARY KEY
		)`)

	_, err := f.check(t, "in_schema")
	requireCopySwapCause(t, err, preflight.CopySwapCausePublicationMember)
	_, err = other.check(t, "elsewhere")
	require.NoError(t, err, "a schema publication does not publish tables of other schemas")
}

// A subscription applying into the table binds it by OID: after the swap
// the apply worker would find no state for the new relation and skip its
// changes. The publisher is another database on the same cluster, with the
// slot created ahead of the subscription as same-cluster replication
// requires; a sibling table the subscription does not apply into is
// accepted.
func TestCheckCopySwapShapeRefusesSubscriptionTarget(t *testing.T) {
	serverURL := testutil.StartPostgresWithSettings(t, "wal_level=logical")
	publisherURL := testutil.NewDatabase(t, serverURL)
	publisher, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: publisherURL})
	require.NoError(t, err)
	t.Cleanup(publisher.Close)
	_, err = publisher.Exec(t.Context(), `
		CREATE TABLE public.applied (
			id bigint PRIMARY KEY
		)`)
	require.NoError(t, err)
	_, err = publisher.Exec(t.Context(), `CREATE PUBLICATION pub_applied FOR TABLE public.applied`)
	require.NoError(t, err)
	_, err = publisher.Exec(t.Context(), `SELECT pg_create_logical_replication_slot('sub_applied', 'pgoutput')`)
	require.NoError(t, err)

	subscriberURL := testutil.NewDatabase(t, serverURL)
	subscriber, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: subscriberURL})
	require.NoError(t, err)
	t.Cleanup(subscriber.Close)
	_, err = subscriber.Exec(t.Context(), `
		CREATE TABLE public.applied (
			id bigint PRIMARY KEY
		)`)
	require.NoError(t, err)
	_, err = subscriber.Exec(t.Context(), `
		CREATE TABLE public.sibling (
			id bigint PRIMARY KEY
		)`)
	require.NoError(t, err)
	_, err = subscriber.Exec(t.Context(), `
		CREATE SUBSCRIPTION sub_applied
			CONNECTION '`+sameClusterConnInfo(t, publisherURL)+`'
			PUBLICATION pub_applied
			WITH (create_slot = false, slot_name = 'sub_applied', copy_data = false)`)
	require.NoError(t, err)
	t.Cleanup(func() {
		// The subscription is detached from its slot before it is dropped so
		// the drop needs no publisher connection. Disabling only signals the
		// apply worker to exit, and the publisher's walsender keeps the slot
		// active until that worker's connection closes, so the slot is
		// dropped once it is released rather than racing the worker.
		ctx := context.WithoutCancel(t.Context())
		for _, sql := range []string{
			`ALTER SUBSCRIPTION sub_applied DISABLE`,
			`ALTER SUBSCRIPTION sub_applied SET (slot_name = NONE)`,
			`DROP SUBSCRIPTION sub_applied`,
		} {
			_, err := subscriber.Exec(ctx, sql)
			assert.NoError(t, err, sql)
		}
		const slotReleaseDeadline = 10 * time.Second
		assert.EventuallyWithT(t, func(collect *assert.CollectT) {
			var active bool
			err := publisher.QueryRow(ctx,
				`SELECT active FROM pg_replication_slots WHERE slot_name = 'sub_applied'`).Scan(&active)
			if !assert.NoError(collect, err, "read the slot") {
				return
			}
			assert.False(collect, active, "the apply worker's walsender still holds the slot")
		}, slotReleaseDeadline, 50*time.Millisecond)
		_, err := publisher.Exec(ctx, `SELECT pg_drop_replication_slot('sub_applied')`)
		assert.NoError(t, err)
	})
	f := copySwapShapeFixture{serverURL: subscriberURL, pool: subscriber, schema: "public"}

	_, err = f.check(t, "applied")
	requireCopySwapCause(t, err, preflight.CopySwapCauseSubscriptionTarget)
	_, err = f.check(t, "sibling")
	require.NoError(t, err, "a table the subscription does not apply into is not its target")
}

// sameClusterConnInfo is the keyword/value connection string a subscription
// on this cluster uses to reach databaseURL from the server's own side.
func sameClusterConnInfo(t *testing.T, databaseURL string) string {
	t.Helper()
	cfg, err := pgx.ParseConfig(databaseURL)
	require.NoError(t, err)
	quote := func(v string) string { return strings.ReplaceAll(v, "'", "''") }
	return fmt.Sprintf("host=localhost port=5432 dbname=%s user=%s password=%s",
		quote(cfg.Database), quote(cfg.User), quote(cfg.Password))
}

// Objects the named causes do not cover can still depend on the table's OID
// or its row type, and the swap carries none of them: a rule on another
// table writing into it, a SQL-standard function body reading it, a policy
// on another table whose expression consults it, and a column of its row
// type each follow the retained old table. Each is refused with the
// description the catalog gives the dependent.
func TestCheckCopySwapShapeRefusesDependentsTheSwapDoesNotCarry(t *testing.T) {
	f := newCopySwapShapeFixture(t)
	f.exec(t, `
		CREATE TABLE %s.audit (
			id bigint PRIMARY KEY
		)`)
	f.exec(t, `
		CREATE TABLE %s.source (
			id bigint PRIMARY KEY
		)`)
	f.exec(t, `CREATE RULE log_insert AS ON INSERT TO %s.source DO ALSO INSERT INTO %[1]s.audit (id) VALUES (NEW.id)`)
	f.exec(t, `
		CREATE TABLE %s.counted (
			id bigint PRIMARY KEY
		)`)
	f.exec(t, `
		CREATE FUNCTION %s.count_rows() RETURNS bigint LANGUAGE sql
		BEGIN ATOMIC
			SELECT count(*) FROM %[1]s.counted;
		END`)
	f.exec(t, `
		CREATE TABLE %s.allowlist (
			id bigint PRIMARY KEY
		)`)
	f.exec(t, `
		CREATE TABLE %s.guarded (
			id bigint PRIMARY KEY
		)`)
	f.exec(t, `CREATE POLICY allowlisted ON %s.guarded USING (EXISTS (SELECT 1 FROM %[1]s.allowlist a WHERE a.id = guarded.id))`)
	f.exec(t, `
		CREATE TABLE %s.typed (
			id bigint PRIMARY KEY,
			label text
		)`)
	f.exec(t, `
		CREATE TABLE %s.snapshots (
			id bigint PRIMARY KEY,
			row_copy %[1]s.typed
		)`)

	for table, dependent := range map[string]string{
		"audit":     "rule log_insert on table " + f.schema + ".source",
		"counted":   "function " + f.schema + ".count_rows()",
		"allowlist": "policy allowlisted on table " + f.schema + ".guarded",
		"typed":     "column row_copy of table " + f.schema + ".snapshots",
	} {
		_, err := f.check(t, table)
		requireCopySwapCause(t, err, preflight.CopySwapCauseDependents)
		var shapeErr *preflight.UnsupportedCopySwapShapeError
		require.ErrorAs(t, err, &shapeErr)
		assert.Contains(t, shapeErr.Detail, dependent, table)
	}
}

// The table's own objects — its identity sequence, indexes, constraints,
// defaults, and policies — also depend on its OID, through edges the swap
// carries or recreates; they are not dependents, and neither is an object
// of another table that happens to live beside it.
func TestCheckCopySwapShapeAcceptsTheTablesOwnObjects(t *testing.T) {
	f := newCopySwapShapeFixture(t)
	f.exec(t, `
		CREATE TABLE %s.owned (
			id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			amount numeric NOT NULL DEFAULT 0 CHECK (amount >= 0),
			owner_name text NOT NULL DEFAULT current_user,
			lower_name text GENERATED ALWAYS AS (lower(owner_name)) STORED
		)`)
	f.exec(t, `CREATE INDEX owned_lower_name_idx ON %s.owned (lower_name) WHERE amount > 0`)
	f.exec(t, `ALTER TABLE %s.owned ENABLE ROW LEVEL SECURITY`)
	f.exec(t, `CREATE POLICY own_rows ON %s.owned USING (owner_name = current_user)`)
	f.exec(t, `CREATE STATISTICS %s.owned_stats ON amount, owner_name FROM %[1]s.owned`)
	f.exec(t, `
		CREATE TABLE %s.neighbour (
			id bigint PRIMARY KEY
		)`)
	f.exec(t, `CREATE VIEW %s.neighbour_view AS SELECT id FROM %[1]s.neighbour`)

	_, err := f.check(t, "owned")
	require.NoError(t, err)
}

// The publication read resolves to the real catalog whatever the session's
// search_path says: an impostor pg_publication_tables ahead of pg_catalog
// that publishes nothing does not hide a FOR ALL TABLES publication.
func TestCheckCopySwapShapeIgnoresTheSessionSearchPath(t *testing.T) {
	f := newCopySwapShapeFixtureInOwnDatabase(t)
	f.exec(t, `
		CREATE TABLE %s.published (
			id bigint PRIMARY KEY
		)`)
	_, err := f.pool.Exec(t.Context(), `CREATE PUBLICATION pub_all FOR ALL TABLES`)
	require.NoError(t, err)
	f.exec(t, `
		CREATE VIEW %s.pg_publication_tables AS
		SELECT pubname, schemaname, tablename
		FROM pg_catalog.pg_publication_tables
		WHERE false`)
	shadowing := testutil.NewCatalogShadowingPool(t, f.serverURL, f.schema)

	role, err := preflight.CheckPrivileges(t.Context(), shadowing, f.schema, "published", preflight.Requirement{Tier: preflight.TierCopyAndSwap})
	require.NoError(t, err)
	_, err = preflight.CheckCopySwapShape(t.Context(), shadowing, f.schema, "published", role)
	requireCopySwapCause(t, err, preflight.CopySwapCausePublicationMember)
}

// database is the catalog's name for the database the fixture pool is on.
func (f copySwapShapeFixture) database(t *testing.T) string {
	t.Helper()
	var name string
	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT current_database()`).Scan(&name))
	return name
}

// major is the PostgreSQL major version under test.
func major(t *testing.T) int {
	t.Helper()
	v, err := strconv.Atoi(testutil.PGVersion())
	require.NoError(t, err)
	return v
}

// newCopySwapShapeFixtureInOwnDatabase is newCopySwapShapeFixture on a
// throwaway database, for fixtures whose database-wide objects must not
// touch the tables other tests share.
func newCopySwapShapeFixtureInOwnDatabase(t *testing.T) copySwapShapeFixture {
	t.Helper()
	serverURL := testutil.StartPostgres(t)
	databaseURL := testutil.NewDatabase(t, serverURL)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: databaseURL})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return copySwapShapeFixture{serverURL: databaseURL, pool: pool, schema: testutil.NewSchema(t, pool)}
}

// A proof verified below the copy-and-swap tier cannot mint a shape proof: the
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
