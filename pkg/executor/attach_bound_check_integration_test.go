package executor

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/statement"
)

// These tests prove the rendered proofs against the server: each validated
// CHECK makes the ATTACH skip its scan. The server reports the skip only at
// DEBUG1, so the attach runs on a session of its own with
// client_min_messages = debug1 and the test reads the notices it gets:
// "partition constraint for table ... is implied by existing constraints"
// for the attached table, "updated partition constraint for default
// partition ... is implied by existing constraints" for the DEFAULT
// partition, and "verifying table ..." for each scan. The control test
// shows the same session reports both scans when the proofs are missing.
// The server reports the skip only as message text, which carries no
// SQLSTATE; a major that rewords it fails these tests on every case, and
// the matching strings here are what to update.

type attachProofFixture struct {
	pool   *pgxpool.Pool
	url    string
	schema string
}

func withAttachProofServer(t *testing.T, run func(t *testing.T, newFixture func(t *testing.T) attachProofFixture)) {
	t.Helper()
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	run(t, func(t *testing.T) attachProofFixture {
		t.Helper()
		return attachProofFixture{pool: pool, url: url, schema: testutil.NewSchema(t, pool)}
	})
}

// exec runs sql with %[1]s standing for the fixture's schema.
func (f attachProofFixture) exec(t *testing.T, sql string) {
	t.Helper()
	_, err := f.pool.Exec(t.Context(), fmt.Sprintf(sql, f.schema))
	require.NoError(t, err)
}

// derive renders the proofs of an ATTACH PARTITION statement against the
// parent it names.
func (f attachProofFixture) derive(t *testing.T, attachSQL string) (check, exclusion string, err error) {
	t.Helper()
	attach, ok, err := statement.AttachedPartition(fmt.Sprintf(attachSQL, f.schema))
	require.NoError(t, err)
	require.True(t, ok)
	var parentOID uint32
	require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT $1::regclass::oid",
		pgx.Identifier{attach.ParentSchema, attach.Parent}.Sanitize()).Scan(&parentOID))
	return deriveAttachBoundCheck(t.Context(), briefTx{pool: f.pool}, parentOID, attach.Bound)
}

// prove adds the expression to the table as a CHECK, NOT VALID, and
// validates it: the online way the flow adds a proof.
func (f attachProofFixture) prove(t *testing.T, table, constraint, expr string) {
	t.Helper()
	name := pgx.Identifier{f.schema, table}.Sanitize()
	_, err := f.pool.Exec(t.Context(), "ALTER TABLE "+name+" ADD CONSTRAINT "+pgx.Identifier{constraint}.Sanitize()+" CHECK ("+expr+") NOT VALID")
	require.NoError(t, err, "add %s: %s", constraint, expr)
	_, err = f.pool.Exec(t.Context(), "ALTER TABLE "+name+" VALIDATE CONSTRAINT "+pgx.Identifier{constraint}.Sanitize())
	require.NoError(t, err, "validate %s", constraint)
}

// attach runs the ATTACH on a session of its own at client_min_messages =
// debug1 and returns the notices the server sent while it ran.
func (f attachProofFixture) attach(t *testing.T, attachSQL string) []string {
	t.Helper()
	cfg, err := pgx.ParseConfig(f.url)
	require.NoError(t, err)
	var notices []string
	cfg.OnNotice = func(_ *pgconn.PgConn, n *pgconn.Notice) { notices = append(notices, n.Message) }
	conn, err := pgx.ConnectConfig(t.Context(), cfg)
	require.NoError(t, err)
	defer func() { assert.NoError(t, conn.Close(t.Context())) }()
	_, err = conn.Exec(t.Context(), "SET client_min_messages = debug1")
	require.NoError(t, err)
	_, err = conn.Exec(t.Context(), fmt.Sprintf(attachSQL, f.schema))
	require.NoError(t, err)
	return notices
}

// proveAndAttach derives the proofs, adds them to the new table and, when
// there is one, the DEFAULT partition, and attaches.
func (f attachProofFixture) proveAndAttach(t *testing.T, newTable, defaultTable, attachSQL string) (check, exclusion string, notices []string) {
	t.Helper()
	check, exclusion, err := f.derive(t, attachSQL)
	require.NoError(t, err)
	f.prove(t, newTable, "attach_bound", check)
	if defaultTable != "" {
		f.prove(t, defaultTable, "attach_default_exclusion", exclusion)
	}
	return check, exclusion, f.attach(t, attachSQL)
}

func requireNoScan(t *testing.T, notices []string, newTable string, defaults ...string) {
	t.Helper()
	want := []string{fmt.Sprintf("partition constraint for table %q is implied by existing constraints", newTable)}
	for _, d := range defaults {
		want = append(want, fmt.Sprintf("updated partition constraint for default partition %q is implied by existing constraints", d))
	}
	for _, w := range want {
		assert.Contains(t, notices, w)
	}
	for _, n := range notices {
		assert.False(t, strings.HasPrefix(n, "verifying table"), "the attach scanned a table: %s", n)
	}
}

func TestAttachBoundCheckLetsAttachSkipItsScans(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		// The control: with no proof, the same session reports both scans,
		// so the notices tell a skipped scan from a run one.
		t.Run("control: without proofs the attach scans both tables", func(t *testing.T) {
			f := newFixture(t)
			f.exec(t, `
				CREATE TABLE %[1]s.events (
					id bigint NOT NULL,
					day date
				) PARTITION BY RANGE (day);
				CREATE TABLE %[1]s.events_default PARTITION OF %[1]s.events DEFAULT;
				CREATE TABLE %[1]s.events_2027_01 (
					id bigint NOT NULL,
					day date
				)`)
			notices := f.attach(t, `ALTER TABLE %[1]s.events
				ATTACH PARTITION %[1]s.events_2027_01
				FOR VALUES FROM ('2027-01-01') TO ('2027-02-01')`)
			assert.Contains(t, notices, `verifying table "events_2027_01"`)
			assert.Contains(t, notices, `verifying table "events_default"`)
		})

		// A nullable date key: the CHECK states IS NOT NULL, which the
		// partition constraint includes.
		t.Run("range on one column", func(t *testing.T) {
			f := newFixture(t)
			f.exec(t, `
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
				INSERT INTO %[1]s.events_2027_01 VALUES (3, '2027-01-01'), (4, '2027-01-31')`)
			check, exclusion, notices := f.proveAndAttach(t, "events_2027_01", "events_default", `ALTER TABLE %[1]s.events
				ATTACH PARTITION %[1]s.events_2027_01
				FOR VALUES FROM ('2027-01-01') TO ('2027-02-01')`)
			assert.Equal(t, `"day" IS NOT NULL`+
				` AND "day" OPERATOR("pg_catalog".>=) CAST('2027-01-01' AS date)`+
				` AND "day" OPERATOR("pg_catalog".<) CAST('2027-02-01' AS date)`, check)
			assert.Equal(t, `NOT ("day" IS NOT NULL`+
				` AND "day" OPERATOR("pg_catalog".>=) CAST('2027-01-01' AS date)`+
				` AND "day" OPERATOR("pg_catalog".<) CAST('2027-02-01' AS date))`, exclusion)
			requireNoScan(t, notices, "events_2027_01", "events_default")
		})

		// (tenant, seq) from (1, 10) to (4, 20): the DEFAULT partition holds
		// (2, NULL). Its tenant alone satisfies both sides' arms, and it
		// still belongs in the DEFAULT partition, because a NULL in any
		// range key column routes a row there. The exclusion negates the
		// IS NOT NULL terms with the rest, as the server's DEFAULT
		// constraint does, so it validates over that row.
		t.Run("range on two columns keeps a NULL-key row in the DEFAULT partition", func(t *testing.T) {
			f := newFixture(t)
			f.exec(t, `
				CREATE TABLE %[1]s.ledger (
					tenant integer,
					seq integer
				) PARTITION BY RANGE (tenant, seq);
				CREATE TABLE %[1]s.ledger_default PARTITION OF %[1]s.ledger DEFAULT;
				CREATE TABLE %[1]s.ledger_next (
					tenant integer,
					seq integer
				);
				INSERT INTO %[1]s.ledger VALUES (2, NULL), (5, 1);
				INSERT INTO %[1]s.ledger_next VALUES (1, 10), (3, 1)`)
			_, _, notices := f.proveAndAttach(t, "ledger_next", "ledger_default", `ALTER TABLE %[1]s.ledger
				ATTACH PARTITION %[1]s.ledger_next
				FOR VALUES FROM (1, 10) TO (4, 20)`)
			requireNoScan(t, notices, "ledger_next", "ledger_default")
		})

		// 150 values, past the size up to which the implication proof
		// expands an array element by element: the CHECK carries the
		// server's array whole, so the proof still matches it. One value
		// needs quoting and one holds a backslash, which quote_literal
		// writes as an E'' string.
		t.Run("list of 150 values on a text key, some needing quoting", func(t *testing.T) {
			f := newFixture(t)
			values := []string{`'o''brien'`, `'back\slash'`}
			for i := range 148 {
				values = append(values, fmt.Sprintf("'customer_%d'", i))
			}
			f.exec(t, `
				CREATE TABLE %[1]s.accounts (
					id bigint NOT NULL,
					owner text
				) PARTITION BY LIST (owner);
				CREATE TABLE %[1]s.accounts_default PARTITION OF %[1]s.accounts DEFAULT;
				CREATE TABLE %[1]s.accounts_named (
					id bigint NOT NULL,
					owner text
				);
				INSERT INTO %[1]s.accounts VALUES (1, 'someone else'), (2, NULL);
				INSERT INTO %[1]s.accounts_named VALUES (3, 'o''brien'), (4, 'back\slash'), (5, 'customer_7')`)
			check, _, notices := f.proveAndAttach(t, "accounts_named", "accounts_default", `ALTER TABLE %[1]s.accounts
				ATTACH PARTITION %[1]s.accounts_named
				FOR VALUES IN (`+strings.Join(values, ", ")+`)`)
			assert.Contains(t, check, `CAST('o''brien' AS text)`)
			assert.Contains(t, check, `CAST(E'back\\slash' AS text)`)
			requireNoScan(t, notices, "accounts_named", "accounts_default")
		})

		// A varchar key compares with text's operator, so the CHECK's array
		// is coerced to text[] and no longer matches the server's whole;
		// at 100 values the proof still tests every element one by one.
		t.Run("list of 100 values on a varchar key", func(t *testing.T) {
			f := newFixture(t)
			values := make([]string, 0, 100)
			for i := range 100 {
				values = append(values, fmt.Sprintf("'customer_%d'", i))
			}
			f.exec(t, `
				CREATE TABLE %[1]s.accounts (
					id bigint NOT NULL,
					owner character varying(20)
				) PARTITION BY LIST (owner);
				CREATE TABLE %[1]s.accounts_default PARTITION OF %[1]s.accounts DEFAULT;
				CREATE TABLE %[1]s.accounts_named (
					id bigint NOT NULL,
					owner character varying(20)
				);
				INSERT INTO %[1]s.accounts VALUES (1, 'someone else'), (2, NULL);
				INSERT INTO %[1]s.accounts_named VALUES (3, 'customer_7')`)
			check, _, notices := f.proveAndAttach(t, "accounts_named", "accounts_default", `ALTER TABLE %[1]s.accounts
				ATTACH PARTITION %[1]s.accounts_named
				FOR VALUES IN (`+strings.Join(values, ", ")+`)`)
			assert.Contains(t, check, `CAST('customer_99' AS character varying(20))`)
			requireNoScan(t, notices, "accounts_named", "accounts_default")
		})

		// A timestamptz key: the bound and the CHECK's cast read the
		// literal in the same session time zone, so they name the same
		// instant.
		t.Run("range on timestamp with time zone", func(t *testing.T) {
			f := newFixture(t)
			f.exec(t, `
				CREATE TABLE %[1]s.audit (
					id bigint NOT NULL,
					at timestamp with time zone
				) PARTITION BY RANGE (at);
				CREATE TABLE %[1]s.audit_default PARTITION OF %[1]s.audit DEFAULT;
				CREATE TABLE %[1]s.audit_2027_01 (
					id bigint NOT NULL,
					at timestamp with time zone
				);
				INSERT INTO %[1]s.audit VALUES (1, '2026-12-31 23:00+00'), (2, NULL);
				INSERT INTO %[1]s.audit_2027_01 VALUES (3, '2027-01-15 12:00+00')`)
			check, _, notices := f.proveAndAttach(t, "audit_2027_01", "audit_default", `ALTER TABLE %[1]s.audit
				ATTACH PARTITION %[1]s.audit_2027_01
				FOR VALUES FROM ('2027-01-01 00:00+00') TO ('2027-02-01 00:00+00')`)
			assert.Contains(t, check, `CAST('2027-01-01 00:00+00' AS timestamp with time zone)`)
			requireNoScan(t, notices, "audit_2027_01", "audit_default")
		})

		// (tenant, seq) from (1, 10) to (2, 20): the OR arms the server
		// writes, not a row comparison.
		t.Run("range on two columns", func(t *testing.T) {
			f := newFixture(t)
			f.exec(t, `
				CREATE TABLE %[1]s.ledger (
					tenant integer,
					seq integer
				) PARTITION BY RANGE (tenant, seq);
				CREATE TABLE %[1]s.ledger_default PARTITION OF %[1]s.ledger DEFAULT;
				CREATE TABLE %[1]s.ledger_next (
					tenant integer,
					seq integer
				);
				INSERT INTO %[1]s.ledger VALUES (1, 9), (2, 20), (NULL, 15);
				INSERT INTO %[1]s.ledger_next VALUES (1, 10), (1, 99), (2, 19)`)
			check, _, notices := f.proveAndAttach(t, "ledger_next", "ledger_default", `ALTER TABLE %[1]s.ledger
				ATTACH PARTITION %[1]s.ledger_next
				FOR VALUES FROM (1, 10) TO (2, 20)`)
			assert.Equal(t, `"tenant" IS NOT NULL AND "seq" IS NOT NULL`+
				` AND ("tenant" OPERATOR("pg_catalog".>) CAST('1' AS integer)`+
				` OR ("tenant" OPERATOR("pg_catalog".=) CAST('1' AS integer) AND "seq" OPERATOR("pg_catalog".>=) CAST('10' AS integer)))`+
				` AND ("tenant" OPERATOR("pg_catalog".<) CAST('2' AS integer)`+
				` OR ("tenant" OPERATOR("pg_catalog".=) CAST('2' AS integer) AND "seq" OPERATOR("pg_catalog".<) CAST('20' AS integer)))`, check)
			requireNoScan(t, notices, "ledger_next", "ledger_default")
		})

		// From (5, 10) to (5, 20): the server writes tenant = 5. The values
		// are compared by the key's equality operator, so 5 and 05 are equal.
		t.Run("range with an equal leading column", func(t *testing.T) {
			f := newFixture(t)
			f.exec(t, `
				CREATE TABLE %[1]s.ledger (
					tenant integer,
					seq integer
				) PARTITION BY RANGE (tenant, seq);
				CREATE TABLE %[1]s.ledger_default PARTITION OF %[1]s.ledger DEFAULT;
				CREATE TABLE %[1]s.ledger_5 (
					tenant integer,
					seq integer
				);
				INSERT INTO %[1]s.ledger VALUES (5, 9), (6, 15);
				INSERT INTO %[1]s.ledger_5 VALUES (5, 10), (5, 19)`)
			check, _, notices := f.proveAndAttach(t, "ledger_5", "ledger_default", `ALTER TABLE %[1]s.ledger
				ATTACH PARTITION %[1]s.ledger_5
				FOR VALUES FROM (5, 10) TO ('05', 20)`)
			assert.Equal(t, `"tenant" IS NOT NULL AND "seq" IS NOT NULL`+
				` AND "tenant" OPERATOR("pg_catalog".=) CAST('5' AS integer)`+
				` AND "seq" OPERATOR("pg_catalog".>=) CAST('10' AS integer)`+
				` AND "seq" OPERATOR("pg_catalog".<) CAST('20' AS integer)`, check)
			requireNoScan(t, notices, "ledger_5", "ledger_default")
		})

		// From (1, MINVALUE) to (2, MAXVALUE) is tenant between 1 and 2,
		// inclusive; the second column bounds nothing.
		t.Run("range with MINVALUE and MAXVALUE", func(t *testing.T) {
			f := newFixture(t)
			f.exec(t, `
				CREATE TABLE %[1]s.ledger (
					tenant integer,
					seq integer
				) PARTITION BY RANGE (tenant, seq);
				CREATE TABLE %[1]s.ledger_default PARTITION OF %[1]s.ledger DEFAULT;
				CREATE TABLE %[1]s.ledger_1_2 (
					tenant integer,
					seq integer
				);
				INSERT INTO %[1]s.ledger VALUES (0, 1), (3, 1);
				INSERT INTO %[1]s.ledger_1_2 VALUES (1, -5), (2, 500)`)
			check, _, notices := f.proveAndAttach(t, "ledger_1_2", "ledger_default", `ALTER TABLE %[1]s.ledger
				ATTACH PARTITION %[1]s.ledger_1_2
				FOR VALUES FROM (1, MINVALUE) TO (2, MAXVALUE)`)
			assert.Equal(t, `"tenant" IS NOT NULL AND "seq" IS NOT NULL`+
				` AND "tenant" OPERATOR("pg_catalog".>=) CAST('1' AS integer)`+
				` AND "tenant" OPERATOR("pg_catalog".<=) CAST('2' AS integer)`, check)
			requireNoScan(t, notices, "ledger_1_2", "ledger_default")
		})

		t.Run("list without NULL", func(t *testing.T) {
			f := newFixture(t)
			f.exec(t, `
				CREATE TABLE %[1]s.orders (
					id bigint NOT NULL,
					region text
				) PARTITION BY LIST (region);
				CREATE TABLE %[1]s.orders_default PARTITION OF %[1]s.orders DEFAULT;
				CREATE TABLE %[1]s.orders_eu (
					id bigint NOT NULL,
					region text
				);
				INSERT INTO %[1]s.orders VALUES (1, 'apac'), (2, NULL);
				INSERT INTO %[1]s.orders_eu VALUES (3, 'eu'), (4, 'uk')`)
			check, exclusion, notices := f.proveAndAttach(t, "orders_eu", "orders_default", `ALTER TABLE %[1]s.orders
				ATTACH PARTITION %[1]s.orders_eu
				FOR VALUES IN ('eu', 'uk')`)
			assert.Equal(t, `"region" IS NOT NULL AND "region" OPERATOR("pg_catalog".=) ANY (ARRAY[CAST('eu' AS text), CAST('uk' AS text)])`, check)
			assert.Equal(t, `NOT ("region" OPERATOR("pg_catalog".=) ANY (ARRAY[CAST('eu' AS text), CAST('uk' AS text)]))`, exclusion)
			requireNoScan(t, notices, "orders_eu", "orders_default")
		})

		// NULL listed: the new partition takes NULL keys, so the DEFAULT
		// partition's exclusion forbids them there too.
		t.Run("list with NULL", func(t *testing.T) {
			f := newFixture(t)
			f.exec(t, `
				CREATE TABLE %[1]s.orders (
					id bigint NOT NULL,
					region text
				) PARTITION BY LIST (region);
				CREATE TABLE %[1]s.orders_default PARTITION OF %[1]s.orders DEFAULT;
				CREATE TABLE %[1]s.orders_unknown (
					id bigint NOT NULL,
					region text
				);
				INSERT INTO %[1]s.orders VALUES (1, 'apac');
				INSERT INTO %[1]s.orders_unknown VALUES (2, NULL), (3, 'unknown')`)
			check, _, notices := f.proveAndAttach(t, "orders_unknown", "orders_default", `ALTER TABLE %[1]s.orders
				ATTACH PARTITION %[1]s.orders_unknown
				FOR VALUES IN (NULL, 'unknown')`)
			assert.Equal(t, `("region" IS NULL OR "region" OPERATOR("pg_catalog".=) ANY (ARRAY[CAST('unknown' AS text)]))`, check)
			requireNoScan(t, notices, "orders_unknown", "orders_default")
		})

		// An enum key outside pg_catalog: format_type names its schema, and
		// the enum's = is pg_catalog's anyenum operator.
		t.Run("list of an enum", func(t *testing.T) {
			f := newFixture(t)
			f.exec(t, `
				CREATE TYPE %[1]s.status AS ENUM ('new', 'paid', 'void');
				CREATE TABLE %[1]s.invoices (
					id bigint NOT NULL,
					status %[1]s.status
				) PARTITION BY LIST (status);
				CREATE TABLE %[1]s.invoices_default PARTITION OF %[1]s.invoices DEFAULT;
				CREATE TABLE %[1]s.invoices_void (
					id bigint NOT NULL,
					status %[1]s.status
				);
				INSERT INTO %[1]s.invoices VALUES (1, 'new');
				INSERT INTO %[1]s.invoices_void VALUES (2, 'void')`)
			check, _, notices := f.proveAndAttach(t, "invoices_void", "invoices_default", `ALTER TABLE %[1]s.invoices
				ATTACH PARTITION %[1]s.invoices_void
				FOR VALUES IN ('void')`)
			assert.Equal(t, fmt.Sprintf(`"status" IS NOT NULL AND "status" OPERATOR("pg_catalog".=) ANY (ARRAY[CAST('void' AS %s.status)])`, f.schema), check)
			requireNoScan(t, notices, "invoices_void", "invoices_default")
		})

		// Hash on two integer columns: the server's own satisfies_hash_partition
		// test, matched expression for expression.
		t.Run("hash", func(t *testing.T) {
			f := newFixture(t)
			f.exec(t, `
				CREATE TABLE %[1]s.accounts (
					id bigint,
					shard integer
				) PARTITION BY HASH (id, shard);
				CREATE TABLE %[1]s.accounts_p1 (
					id bigint,
					shard integer
				);
				INSERT INTO %[1]s.accounts_p1
					SELECT id, shard FROM generate_series(1::bigint, 200) id, generate_series(1, 3) shard
					 WHERE satisfies_hash_partition('%[1]s.accounts'::regclass, 4, 1, id, shard)`)
			check, exclusion, notices := f.proveAndAttach(t, "accounts_p1", "", `ALTER TABLE %[1]s.accounts
				ATTACH PARTITION %[1]s.accounts_p1
				FOR VALUES WITH (MODULUS 4, REMAINDER 1)`)
			assert.Equal(t, fmt.Sprintf(`pg_catalog.satisfies_hash_partition(CAST('%s.accounts' AS pg_catalog.regclass), 4, 1, "id", "shard")`, f.schema), check)
			assert.Empty(t, exclusion)
			requireNoScan(t, notices, "accounts_p1")
		})

		// Expression keys use the key expression as the server prints it.
		t.Run("list on an expression", func(t *testing.T) {
			f := newFixture(t)
			f.exec(t, `
				CREATE TABLE %[1]s.users (
					id bigint NOT NULL,
					name text
				) PARTITION BY LIST (lower(name));
				CREATE TABLE %[1]s.users_default PARTITION OF %[1]s.users DEFAULT;
				CREATE TABLE %[1]s.users_a (
					id bigint NOT NULL,
					name text
				);
				INSERT INTO %[1]s.users VALUES (1, 'Bob');
				INSERT INTO %[1]s.users_a VALUES (2, 'A'), (3, 'a')`)
			check, _, notices := f.proveAndAttach(t, "users_a", "users_default", `ALTER TABLE %[1]s.users
				ATTACH PARTITION %[1]s.users_a
				FOR VALUES IN ('a')`)
			assert.Equal(t, `(lower(name)) IS NOT NULL AND (lower(name)) OPERATOR("pg_catalog".=) ANY (ARRAY[CAST('a' AS text)])`, check)
			requireNoScan(t, notices, "users_a", "users_default")
		})

		t.Run("range on an expression", func(t *testing.T) {
			f := newFixture(t)
			f.exec(t, `
				CREATE TABLE %[1]s.points (
					x integer,
					y integer
				) PARTITION BY RANGE ((x + y));
				CREATE TABLE %[1]s.points_default PARTITION OF %[1]s.points DEFAULT;
				CREATE TABLE %[1]s.points_low (
					x integer,
					y integer
				);
				INSERT INTO %[1]s.points VALUES (10, 10);
				INSERT INTO %[1]s.points_low VALUES (1, 2), (4, 5)`)
			check, _, notices := f.proveAndAttach(t, "points_low", "points_default", `ALTER TABLE %[1]s.points
				ATTACH PARTITION %[1]s.points_low
				FOR VALUES FROM (0) TO (10)`)
			assert.Equal(t, `(((x + y))) IS NOT NULL`+
				` AND (((x + y))) OPERATOR("pg_catalog".>=) CAST('0' AS integer)`+
				` AND (((x + y))) OPERATOR("pg_catalog".<) CAST('10' AS integer)`, check)
			requireNoScan(t, notices, "points_low", "points_default")
		})

		// A sub-partitioned DEFAULT partition is checked leaf by leaf, so the
		// exclusion proves the bound on each leaf.
		t.Run("sub-partitioned DEFAULT, exclusion on each leaf", func(t *testing.T) {
			f := newFixture(t)
			f.exec(t, `
				CREATE TABLE %[1]s.events (
					id bigint NOT NULL,
					day date
				) PARTITION BY RANGE (day);
				CREATE TABLE %[1]s.events_default PARTITION OF %[1]s.events DEFAULT PARTITION BY HASH (id);
				CREATE TABLE %[1]s.events_default_0 PARTITION OF %[1]s.events_default FOR VALUES WITH (MODULUS 2, REMAINDER 0);
				CREATE TABLE %[1]s.events_default_1 PARTITION OF %[1]s.events_default FOR VALUES WITH (MODULUS 2, REMAINDER 1);
				CREATE TABLE %[1]s.events_2027_01 (
					id bigint NOT NULL,
					day date
				);
				INSERT INTO %[1]s.events VALUES (1, '2026-12-31'), (2, '2027-02-01')`)
			attachSQL := `ALTER TABLE %[1]s.events
				ATTACH PARTITION %[1]s.events_2027_01
				FOR VALUES FROM ('2027-01-01') TO ('2027-02-01')`
			check, exclusion, err := f.derive(t, attachSQL)
			require.NoError(t, err)
			f.prove(t, "events_2027_01", "attach_bound", check)
			f.prove(t, "events_default_0", "attach_default_exclusion", exclusion)
			f.prove(t, "events_default_1", "attach_default_exclusion", exclusion)
			requireNoScan(t, f.attach(t, attachSQL), "events_2027_01", "events_default_0", "events_default_1")
		})
	})
}

// Keys the server's proof cannot match are refused from the catalog,
// before anything is added: a hash key of a collatable type, whose
// partition constraint carries no input collation, and a key with an
// explicit COLLATE.
func TestAttachBoundCheckRefusesUnprovableKeys(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		t.Run("hash on text", func(t *testing.T) {
			f := newFixture(t)
			f.exec(t, `
				CREATE TABLE %[1]s.tags (
					name text
				) PARTITION BY HASH (name);
				CREATE TABLE %[1]s.tags_p0 (
					name text
				)`)
			_, _, err := f.derive(t, `ALTER TABLE %[1]s.tags
				ATTACH PARTITION %[1]s.tags_p0
				FOR VALUES WITH (MODULUS 2, REMAINDER 0)`)
			require.ErrorIs(t, err, errAttachBoundUnprovable)
		})
		t.Run("explicit COLLATE", func(t *testing.T) {
			f := newFixture(t)
			f.exec(t, `
				CREATE TABLE %[1]s.tags (
					name text
				) PARTITION BY LIST (name COLLATE "C");
				CREATE TABLE %[1]s.tags_a (
					name text
				)`)
			_, _, err := f.derive(t, `ALTER TABLE %[1]s.tags
				ATTACH PARTITION %[1]s.tags_a
				FOR VALUES IN ('a')`)
			require.ErrorIs(t, err, errAttachBoundUnprovable)
		})
		// Past 100 values the proof needs the CHECK's array to match the
		// server's whole, and a varchar key's array is coerced to text[].
		t.Run("list of 101 values on a varchar key", func(t *testing.T) {
			f := newFixture(t)
			values := make([]string, 0, 101)
			for i := range 101 {
				values = append(values, fmt.Sprintf("'customer_%d'", i))
			}
			f.exec(t, `
				CREATE TABLE %[1]s.accounts (
					owner character varying(20)
				) PARTITION BY LIST (owner);
				CREATE TABLE %[1]s.accounts_named (
					owner character varying(20)
				)`)
			_, _, err := f.derive(t, `ALTER TABLE %[1]s.accounts
				ATTACH PARTITION %[1]s.accounts_named
				FOR VALUES IN (`+strings.Join(values, ", ")+`)`)
			require.ErrorIs(t, err, errAttachBoundUnprovable)
		})
		// The attach proves the bound ANDed with the parent's own
		// partition constraint (region = 'eu'), which a CHECK of the bound
		// alone does not imply, so the leaf's attach would scan anyway.
		t.Run("parent that is itself a partition", func(t *testing.T) {
			f := newFixture(t)
			f.exec(t, `
				CREATE TABLE %[1]s.orders (
					id bigint NOT NULL,
					region text,
					day date
				) PARTITION BY LIST (region);
				CREATE TABLE %[1]s.orders_eu PARTITION OF %[1]s.orders
					FOR VALUES IN ('eu')
					PARTITION BY RANGE (day);
				CREATE TABLE %[1]s.orders_eu_2027_01 (
					id bigint NOT NULL,
					region text,
					day date
				)`)
			_, _, err := f.derive(t, `ALTER TABLE %[1]s.orders_eu
				ATTACH PARTITION %[1]s.orders_eu_2027_01
				FOR VALUES FROM ('2027-01-01') TO ('2027-02-01')`)
			require.ErrorIs(t, err, errAttachBoundUnprovable)
		})
		// The server decides a range's equal prefix under the key's
		// collation; a nondeterministic one can call 'a' and 'A' equal.
		t.Run("nondeterministic collation", func(t *testing.T) {
			f := newFixture(t)
			f.exec(t, `
				CREATE COLLATION %[1]s.case_insensitive (
					provider = icu,
					locale = 'und-u-ks-level2',
					deterministic = false
				);
				CREATE TABLE %[1]s.tags (
					name text COLLATE %[1]s.case_insensitive
				) PARTITION BY RANGE (name);
				CREATE TABLE %[1]s.tags_a (
					name text COLLATE %[1]s.case_insensitive
				)`)
			_, _, err := f.derive(t, `ALTER TABLE %[1]s.tags
				ATTACH PARTITION %[1]s.tags_a
				FOR VALUES FROM ('a') TO ('b')`)
			require.ErrorIs(t, err, errAttachBoundUnprovable)
		})
		// IS NULL on a composite value tests every field, and the server's
		// partition constraint tests the value itself.
		t.Run("composite key", func(t *testing.T) {
			f := newFixture(t)
			f.exec(t, `
				CREATE TYPE %[1]s.version AS (
					major integer,
					minor integer
				);
				CREATE TABLE %[1]s.releases (
					v %[1]s.version
				) PARTITION BY RANGE (v);
				CREATE TABLE %[1]s.releases_1 (
					v %[1]s.version
				)`)
			_, _, err := f.derive(t, `ALTER TABLE %[1]s.releases
				ATTACH PARTITION %[1]s.releases_1
				FOR VALUES FROM ('(1,0)') TO ('(2,0)')`)
			require.ErrorIs(t, err, errAttachBoundUnprovable)
		})
		t.Run("cast in the bound", func(t *testing.T) {
			f := newFixture(t)
			f.exec(t, `
				CREATE TABLE %[1]s.events (
					day date
				) PARTITION BY RANGE (day);
				CREATE TABLE %[1]s.events_2027_01 (
					day date
				)`)
			_, _, err := f.derive(t, `ALTER TABLE %[1]s.events
				ATTACH PARTITION %[1]s.events_2027_01
				FOR VALUES FROM ('2027-01-01'::date) TO ('2027-02-01')`)
			require.ErrorIs(t, err, errAttachBoundUnprovable)
		})
	})
}
