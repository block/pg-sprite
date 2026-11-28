package executor

import (
	"context"

	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/progress"
)

// These tests run the foreign key step against the server: it gives a table
// every enforced foreign key of the partitioned parent, validated, before
// the attach, and the ATTACH that follows adopts those foreign keys instead
// of creating and validating its own.

// tableFK is one foreign key on a table.
type tableFK struct {
	oid       uint32
	validated bool
	// adopted is whether a parent's foreign key has adopted it.
	adopted bool
}

// tableFKs maps each foreign key on the table to its state.
func (f attachProofFixture) tableFKs(t *testing.T, table string) map[string]tableFK {
	t.Helper()
	rows, err := f.pool.Query(t.Context(), `SELECT conname, oid, convalidated, conparentid <> 0
		FROM pg_catalog.pg_constraint
		WHERE conrelid = $1::regclass AND contype = 'f'`, pgx.Identifier{f.schema, table}.Sanitize())
	require.NoError(t, err)
	defer rows.Close()
	got := map[string]tableFK{}
	for rows.Next() {
		var name string
		var fk tableFK
		require.NoError(t, rows.Scan(&name, &fk.oid, &fk.validated, &fk.adopted))
		got[name] = fk
	}
	require.NoError(t, rows.Err())
	return got
}

// serverMajor is the fixture server's major version.
func (f attachProofFixture) serverMajor(t *testing.T) int {
	t.Helper()
	var major int
	require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT current_setting('server_version_num')::int / 10000").Scan(&major))
	return major
}

// paymentsFKParent is a partitioned table with two foreign keys of
// different rules to two plain tables. payments_2027 is the table to
// attach, with no foreign key and rows that satisfy both.
const paymentsFKParent = `
	CREATE TABLE %[1]s.accounts (
		id bigint PRIMARY KEY
	);
	CREATE TABLE %[1]s.merchants (
		id bigint PRIMARY KEY
	);
	INSERT INTO %[1]s.accounts VALUES (1), (2);
	INSERT INTO %[1]s.merchants VALUES (10);
	CREATE TABLE %[1]s.payments (
		id bigint NOT NULL,
		paid date NOT NULL,
		account_id bigint,
		merchant_id bigint,
		CONSTRAINT payments_account_fk FOREIGN KEY (account_id)
			REFERENCES %[1]s.accounts (id) ON DELETE CASCADE,
		CONSTRAINT payments_merchant_fk FOREIGN KEY (merchant_id)
			REFERENCES %[1]s.merchants (id) DEFERRABLE INITIALLY DEFERRED
	) PARTITION BY RANGE (paid);
	CREATE TABLE %[1]s.payments_2026 PARTITION OF %[1]s.payments
		FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');
	CREATE TABLE %[1]s.payments_2027 (
		id bigint NOT NULL,
		paid date NOT NULL,
		account_id bigint,
		merchant_id bigint
	);
	INSERT INTO %[1]s.payments_2027 VALUES
		(1, '2027-03-01', 1, 10),
		(2, '2027-04-01', 2, NULL)`

const attachPaymentsFK2027 = `ALTER TABLE %[1]s.payments
	ATTACH PARTITION %[1]s.payments_2027
	FOR VALUES FROM ('2027-01-01') TO ('2028-01-01')`

// From a table with no foreign key, the step adds both of the parent's NOT
// VALID and validates them. A rerun finds both and adds nothing. The ATTACH
// then adopts exactly those two, by OID, and creates no foreign key of its
// own, which it would have validated under its ACCESS EXCLUSIVE lock.
func TestEnsureAttachForeignKeysGivesTheTableEveryForeignKeyAndTheAttachAddsNone(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, paymentsFKParent)
		parent, table := f.parentOID(t, "payments"), f.standalone(t, "payments_2027")

		var ran []string
		results, err := ensureAttachForeignKeys(t.Context(), f.pool, parent, table, attachProofTestBudget,
			func(_ progress.Operation, sql string) { ran = append(ran, sql) })
		require.NoError(t, err)
		require.Len(t, results, 2)
		qualified := pgx.Identifier{f.schema, "payments_2027"}.Sanitize()
		accountFK := derivedPartitionIndexName("payments_account_fk", table.oid)
		merchantFK := derivedPartitionIndexName("payments_merchant_fk", table.oid)
		assert.Equal(t, "payments_account_fk", results[0].parentFK)
		assert.Equal(t, leafFKAdded, results[0].action)
		assert.Equal(t, accountFK, results[0].constraint)
		assert.Equal(t, "payments_merchant_fk", results[1].parentFK)
		assert.Equal(t, leafFKAdded, results[1].action)
		assert.Equal(t, merchantFK, results[1].constraint)
		accounts := pgx.Identifier{f.schema, "accounts"}.Sanitize()
		merchants := pgx.Identifier{f.schema, "merchants"}.Sanitize()
		assert.Equal(t, []string{
			"ALTER TABLE " + qualified + " ADD CONSTRAINT " + pgx.Identifier{accountFK}.Sanitize() +
				` FOREIGN KEY ("account_id") REFERENCES ` + accounts + ` ("id") MATCH SIMPLE ON UPDATE NO ACTION ON DELETE CASCADE NOT DEFERRABLE INITIALLY IMMEDIATE NOT VALID`,
			"ALTER TABLE " + qualified + " VALIDATE CONSTRAINT " + pgx.Identifier{accountFK}.Sanitize(),
			"ALTER TABLE " + qualified + " ADD CONSTRAINT " + pgx.Identifier{merchantFK}.Sanitize() +
				` FOREIGN KEY ("merchant_id") REFERENCES ` + merchants + ` ("id") MATCH SIMPLE ON UPDATE NO ACTION ON DELETE NO ACTION DEFERRABLE INITIALLY DEFERRED NOT VALID`,
			"ALTER TABLE " + qualified + " VALIDATE CONSTRAINT " + pgx.Identifier{merchantFK}.Sanitize(),
		}, ran)
		before := f.tableFKs(t, "payments_2027")
		assert.Equal(t, map[string]tableFK{
			accountFK:  {oid: results[0].constraintOID, validated: true},
			merchantFK: {oid: results[1].constraintOID, validated: true},
		}, before)

		again, err := ensureAttachForeignKeys(t.Context(), f.pool, parent, table, attachProofTestBudget, nil)
		require.NoError(t, err)
		for _, r := range again {
			assert.Equal(t, leafFKFound, r.action, "a rerun finds %s's foreign key", r.parentFK)
		}
		assert.Equal(t, before, f.tableFKs(t, "payments_2027"), "a rerun adds nothing")

		f.attach(t, attachPaymentsFK2027)
		after := f.tableFKs(t, "payments_2027")
		require.Len(t, after, 2, "the attach created no foreign key of its own")
		for name, fk := range after {
			assert.Equal(t, before[name].oid, fk.oid, "%s is the foreign key the step gave the table", name)
			assert.True(t, fk.adopted, "%s is adopted by the parent's foreign key", name)
		}
	})
}

// A NOT VALID foreign key of the definition the table already has, under a
// name of its own, is validated where it stands. Without the step, the
// attach before PostgreSQL 18 leaves it and adds a second foreign key it
// validates; from 18 it adopts it and validates it under its lock.
func TestEnsureAttachForeignKeysValidatesTheTablesOwnNotValidForeignKey(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, paymentsFKParent)
		f.exec(t, `
			ALTER TABLE %[1]s.payments_2027
				ADD CONSTRAINT my_account_fk FOREIGN KEY (account_id)
				REFERENCES %[1]s.accounts (id) ON DELETE CASCADE NOT VALID`)
		parent, table := f.parentOID(t, "payments"), f.standalone(t, "payments_2027")
		mine := f.tableFKs(t, "payments_2027")["my_account_fk"]
		require.False(t, mine.validated)

		results, err := ensureAttachForeignKeys(t.Context(), f.pool, parent, table, attachProofTestBudget, nil)
		require.NoError(t, err)
		require.Len(t, results, 2)
		assert.Equal(t, leafFKResult{action: leafFKValidated, constraint: "my_account_fk", constraintOID: mine.oid}, results[0].leafFKResult)

		f.attach(t, attachPaymentsFK2027)
		after := f.tableFKs(t, "payments_2027")
		require.Len(t, after, 2)
		assert.Equal(t, tableFK{oid: mine.oid, validated: true, adopted: true}, after["my_account_fk"])
	})
}

// A row whose account is missing fails the VALIDATE, typed. The foreign key
// stays NOT VALID on the table, refusing new orphan rows, and a rerun once
// the row is fixed validates it where it stands.
func TestEnsureAttachForeignKeysStopsOnAnOrphanRowAndResumes(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, paymentsFKParent)
		f.exec(t, `INSERT INTO %[1]s.payments_2027 VALUES (3, '2027-05-01', 99, NULL)`)
		parent, table := f.parentOID(t, "payments"), f.standalone(t, "payments_2027")
		accountFK := derivedPartitionIndexName("payments_account_fk", table.oid)

		_, err := ensureAttachForeignKeys(t.Context(), f.pool, parent, table, attachProofTestBudget, nil)
		require.ErrorIs(t, err, ErrPartitionedForeignKeyViolation)
		fks := f.tableFKs(t, "payments_2027")
		require.Len(t, fks, 1, "the step stopped before the second foreign key")
		assert.False(t, fks[accountFK].validated)

		f.exec(t, `DELETE FROM %[1]s.payments_2027 WHERE id = 3`)
		results, err := ensureAttachForeignKeys(t.Context(), f.pool, parent, table, attachProofTestBudget, nil)
		require.NoError(t, err)
		require.Len(t, results, 2)
		assert.Equal(t, leafFKValidated, results[0].action)
		assert.Equal(t, fks[accountFK].oid, results[0].constraintOID)
		assert.Equal(t, leafFKAdded, results[1].action)
	})
}

// From PostgreSQL 18 a partitioned table's foreign key can be NOT VALID or
// NOT ENFORCED. The attach still validates a new partition against a NOT
// VALID one, so the step gives the table that foreign key validated. It
// clones a NOT ENFORCED one without a scan, so the step leaves it to the
// attach.
func TestEnsureAttachForeignKeysOnANotValidAndANotEnforcedParentForeignKey(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		if major := f.serverMajor(t); major < 18 {
			t.Skipf("PostgreSQL %d has no NOT VALID or NOT ENFORCED foreign key on a partitioned table", major)
		}
		f.exec(t, `
			CREATE TABLE %[1]s.accounts (
				id bigint PRIMARY KEY
			);
			INSERT INTO %[1]s.accounts VALUES (1);
			CREATE TABLE %[1]s.transfers (
				id bigint NOT NULL,
				sent date NOT NULL,
				from_id bigint,
				to_id bigint
			) PARTITION BY RANGE (sent);
			ALTER TABLE %[1]s.transfers
				ADD CONSTRAINT transfers_from_fk FOREIGN KEY (from_id)
				REFERENCES %[1]s.accounts (id) NOT VALID;
			ALTER TABLE %[1]s.transfers
				ADD CONSTRAINT transfers_to_fk FOREIGN KEY (to_id)
				REFERENCES %[1]s.accounts (id) NOT ENFORCED;
			CREATE TABLE %[1]s.transfers_2027 (
				id bigint NOT NULL,
				sent date NOT NULL,
				from_id bigint,
				to_id bigint
			);
			INSERT INTO %[1]s.transfers_2027 VALUES (1, '2027-03-01', 1, 99)`)
		parent, table := f.parentOID(t, "transfers"), f.standalone(t, "transfers_2027")

		results, err := ensureAttachForeignKeys(t.Context(), f.pool, parent, table, attachProofTestBudget, nil)
		require.NoError(t, err)
		require.Len(t, results, 1, "the NOT ENFORCED foreign key needs nothing")
		assert.Equal(t, "transfers_from_fk", results[0].parentFK)
		assert.Equal(t, leafFKAdded, results[0].action)

		f.attach(t, `ALTER TABLE %[1]s.transfers
			ATTACH PARTITION %[1]s.transfers_2027
			FOR VALUES FROM ('2027-01-01') TO ('2028-01-01')`)
		after := f.tableFKs(t, "transfers_2027")
		require.Len(t, after, 2, "the attach cloned only the NOT ENFORCED foreign key")
		assert.Equal(t, tableFK{oid: results[0].constraintOID, validated: true, adopted: true}, after[results[0].constraint])
	})
}

// Foreign keys the step cannot give the table are refused, typed, before
// anything is added for them, and a table that is already a partition is
// not the standalone table the step works on.
func TestEnsureAttachForeignKeysRefusesAForeignKeyItCannotGive(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		// ON DELETE SET NULL takes a column list from PostgreSQL 15. The
		// leaf step does not render one.
		t.Run("an ON DELETE column list", func(t *testing.T) {
			f := newFixture(t)
			if major := f.serverMajor(t); major < 15 {
				t.Skipf("PostgreSQL %d has no ON DELETE column list", major)
			}
			f.exec(t, `
				CREATE TABLE %[1]s.accounts (
					id bigint PRIMARY KEY
				);
				CREATE TABLE %[1]s.cards (
					id bigint NOT NULL,
					issued date NOT NULL,
					account_id bigint,
					CONSTRAINT cards_account_fk FOREIGN KEY (account_id)
						REFERENCES %[1]s.accounts (id) ON DELETE SET NULL (account_id)
				) PARTITION BY RANGE (issued);
				CREATE TABLE %[1]s.cards_2027 (
					id bigint NOT NULL,
					issued date NOT NULL,
					account_id bigint
				)`)
			_, err := ensureAttachForeignKeys(t.Context(), f.pool, f.parentOID(t, "cards"), f.standalone(t, "cards_2027"), attachProofTestBudget, nil)
			require.ErrorIs(t, err, errAttachForeignKeyUnsupported)
			assert.Empty(t, f.tableFKs(t, "cards_2027"))
		})

		// A foreign key to a partitioned table puts one more foreign key on
		// the referencing table per referenced partition. The step reads
		// past those, and the leaf step refuses the partitioned reference.
		t.Run("a reference to a partitioned table", func(t *testing.T) {
			f := newFixture(t)
			f.exec(t, `
				CREATE TABLE %[1]s.ledgers (
					id bigint NOT NULL,
					region int NOT NULL,
					PRIMARY KEY (id, region)
				) PARTITION BY LIST (region);
				CREATE TABLE %[1]s.ledgers_1 PARTITION OF %[1]s.ledgers FOR VALUES IN (1);
				CREATE TABLE %[1]s.entries (
					id bigint NOT NULL,
					posted date NOT NULL,
					ledger_id bigint,
					ledger_region int,
					CONSTRAINT entries_ledger_fk FOREIGN KEY (ledger_id, ledger_region)
						REFERENCES %[1]s.ledgers (id, region)
				) PARTITION BY RANGE (posted);
				CREATE TABLE %[1]s.entries_2027 (
					id bigint NOT NULL,
					posted date NOT NULL,
					ledger_id bigint,
					ledger_region int
				)`)
			var fks []parentForeignKey
			require.NoError(t, briefTx{pool: f.pool}.read(t.Context(), "read", func(ctx context.Context, q querier) error {
				var err error
				fks, err = readParentForeignKeys(ctx, q, f.parentOID(t, "entries"), f.serverMajor(t))
				return err
			}))
			require.Len(t, fks, 1, "the foreign key for ledgers_1 is read past")
			assert.Equal(t, "ledgers", fks[0].spec.refTable)

			_, err := ensureAttachForeignKeys(t.Context(), f.pool, f.parentOID(t, "entries"), f.standalone(t, "entries_2027"), attachProofTestBudget, nil)
			require.ErrorIs(t, err, ErrPartitionedForeignKeyReferencedTable)
			assert.ErrorContains(t, err, "entries_ledger_fk")
			assert.Empty(t, f.tableFKs(t, "entries_2027"))
		})

		t.Run("a table that is already a partition", func(t *testing.T) {
			f := newFixture(t)
			f.exec(t, paymentsFKParent)
			table := f.standalone(t, "payments_2027")
			f.exec(t, attachPaymentsFK2027)
			_, err := ensureAttachForeignKeys(t.Context(), f.pool, f.parentOID(t, "payments"), table, attachProofTestBudget, nil)
			require.ErrorIs(t, err, ErrInvariantViolation)
		})
	})
}
