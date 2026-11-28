package executor

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests hold the online attach's column check against PostgreSQL's
// own ATTACH, for the column rules that differ by generated and identity
// columns: each first runs the plain ATTACH in a transaction it rolls back,
// then the online attach, which must refuse exactly when the server does,
// before it gives the table anything. Each table already declares the
// parent's NOT NULL columns, so the column under test is the only
// difference the server can refuse on.

// plainAttachRefusal runs the plain ATTACH of orders_2027 in a transaction
// it rolls back, and returns the SQLSTATE the server refused it with, or ""
// when the server attached it.
func (f attachProofFixture) plainAttachRefusal(t *testing.T) string {
	t.Helper()
	tx, err := f.pool.Begin(t.Context())
	require.NoError(t, err)
	defer func() { require.NoError(t, tx.Rollback(t.Context())) }()
	// The statement names orders_2027 unqualified, as the online attach
	// resolves it under the parent's schema.
	_, err = tx.Exec(t.Context(), "SET LOCAL search_path TO "+pgx.Identifier{f.schema}.Sanitize())
	require.NoError(t, err)
	_, err = tx.Exec(t.Context(), fmt.Sprintf(attachOrders2027Online, f.schema))
	if err == nil {
		return ""
	}
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	require.True(t, ok, "%v", err)
	return pgErr.Code
}

// requireOnlineAttachAgrees runs the online attach of orders_2027 and
// requires it to refuse, with nothing added, when the server refused the
// plain ATTACH, and to attach the table when the server did.
func (f attachProofFixture) requireOnlineAttachAgrees(t *testing.T, serverRefusal string) {
	t.Helper()
	rep, err := f.attachOnline(t, f.pool, attachOrders2027Online, attachFlowOptions, RunStep{})
	if serverRefusal == "" {
		require.NoError(t, err)
		assert.True(t, rep.Attached)
		return
	}
	require.ErrorIs(t, err, ErrAttachPartitionTable)
	assert.Equal(t, CodeAttachPartitionTable, OutcomeCode(err))
	assert.Empty(t, rep.Steps)
	f.requireNothingAdded(t, "orders_2027")
}

// A plain column where the parent's is generated is refused by every
// supported server.
func TestAttachPartitionOnlineRefusesAPlainColumnTheParentGenerates(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, `
			CREATE TABLE %[1]s.orders (
				id bigint NOT NULL,
				day date NOT NULL,
				qty int,
				total int GENERATED ALWAYS AS (qty * 10) STORED
			) PARTITION BY RANGE (day);
			CREATE TABLE %[1]s.orders_2027 (
				id bigint NOT NULL,
				day date NOT NULL,
				qty int,
				total int
			)`)

		refusal := f.plainAttachRefusal(t)
		assert.Equal(t, "42804", refusal) // datatype_mismatch
		f.requireOnlineAttachAgrees(t, refusal)
	})
}

// A generated column where the parent's is plain is refused from
// PostgreSQL 16; 14 and 15 attach it.
func TestAttachPartitionOnlineChecksAGeneratedColumnTheParentDoesNotGenerate(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, `
			CREATE TABLE %[1]s.orders (
				id bigint NOT NULL,
				day date NOT NULL,
				qty int,
				total int
			) PARTITION BY RANGE (day);
			CREATE TABLE %[1]s.orders_2027 (
				id bigint NOT NULL,
				day date NOT NULL,
				qty int,
				total int GENERATED ALWAYS AS (qty * 10) STORED
			)`)

		refusal := f.plainAttachRefusal(t)
		if f.serverMajor(t) >= 16 {
			assert.Equal(t, "42804", refusal)
		} else {
			assert.Empty(t, refusal)
		}
		f.requireOnlineAttachAgrees(t, refusal)
	})
}

// A generated column whose expression differs from the parent's is refused
// by PostgreSQL 14 and 15, which compare the two expressions; 16 and later
// attach it, and the partition keeps its own expression.
func TestAttachPartitionOnlineChecksAGenerationExpressionOtherThanTheParents(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, `
			CREATE TABLE %[1]s.orders (
				id bigint NOT NULL,
				day date NOT NULL,
				qty int,
				total int GENERATED ALWAYS AS (qty * 10) STORED
			) PARTITION BY RANGE (day);
			CREATE TABLE %[1]s.orders_2027 (
				id bigint NOT NULL,
				day date NOT NULL,
				qty int,
				total int GENERATED ALWAYS AS (qty * 20) STORED
			)`)

		refusal := f.plainAttachRefusal(t)
		if f.serverMajor(t) >= 16 {
			assert.Empty(t, refusal)
		} else {
			assert.Equal(t, "42804", refusal)
		}
		f.requireOnlineAttachAgrees(t, refusal)
	})
}

// The same generation expression as the parent's attaches on every
// supported server.
func TestAttachPartitionOnlineAttachesTheParentsGenerationExpression(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, `
			CREATE TABLE %[1]s.orders (
				id bigint NOT NULL,
				day date NOT NULL,
				qty int,
				total int GENERATED ALWAYS AS (qty * 10) STORED
			) PARTITION BY RANGE (day);
			CREATE TABLE %[1]s.orders_2027 (
				id bigint NOT NULL,
				day date NOT NULL,
				qty int,
				total int GENERATED ALWAYS AS (qty * 10) STORED
			)`)

		refusal := f.plainAttachRefusal(t)
		assert.Empty(t, refusal)
		f.requireOnlineAttachAgrees(t, refusal)
	})
}

// A table with an identity column is refused from PostgreSQL 17, where a
// partition takes the parent's identity instead; 14 to 16 attach it.
func TestAttachPartitionOnlineChecksAnIdentityColumn(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, `
			CREATE TABLE %[1]s.orders (
				id bigint NOT NULL,
				day date NOT NULL
			) PARTITION BY RANGE (day);
			CREATE TABLE %[1]s.orders_2027 (
				id bigint GENERATED ALWAYS AS IDENTITY,
				day date NOT NULL
			)`)

		refusal := f.plainAttachRefusal(t)
		if f.serverMajor(t) >= 17 {
			assert.Equal(t, "55000", refusal) // object_not_in_prerequisite_state
		} else {
			assert.Empty(t, refusal)
		}
		f.requireOnlineAttachAgrees(t, refusal)
	})
}

// A virtual generated column where the parent's is stored is refused;
// virtual generated columns exist from PostgreSQL 18.
func TestAttachPartitionOnlineRefusesAGeneratedColumnOfTheOtherKind(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		if f.serverMajor(t) < 18 {
			t.Skip("virtual generated columns need PostgreSQL 18")
		}
		f.exec(t, `
			CREATE TABLE %[1]s.orders (
				id bigint NOT NULL,
				day date NOT NULL,
				qty int,
				total int GENERATED ALWAYS AS (qty * 10) STORED
			) PARTITION BY RANGE (day);
			CREATE TABLE %[1]s.orders_2027 (
				id bigint NOT NULL,
				day date NOT NULL,
				qty int,
				total int GENERATED ALWAYS AS (qty * 10) VIRTUAL
			)`)

		refusal := f.plainAttachRefusal(t)
		assert.Equal(t, "42804", refusal)
		f.requireOnlineAttachAgrees(t, refusal)
	})
}
