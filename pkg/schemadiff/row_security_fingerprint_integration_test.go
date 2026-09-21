package schemadiff_test

import (
	"fmt"
	"testing"

	"github.com/block/pg-sprite/pkg/diffplan"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReviewFingerprintRechecksLivePolicy(t *testing.T) {
	pool, schema := rowSecurityTable(t)
	sql := `CREATE TABLE documents (
     id bigint PRIMARY KEY,
     owner_id bigint NOT NULL
 );
 ALTER TABLE documents ENABLE ROW LEVEL SECURITY;
 CREATE POLICY readers ON documents FOR SELECT USING (owner_id = 7);`
	initial := reviewDesired(t, pool, schema, sql)
	desired, err := statement.ParseDesiredWithRowSecurity(sql)
	require.NoError(t, err)
	fresh, err := diffplan.VerifyRowSecurityReview(t.Context(), pool, schema, desired, initial.Fingerprint)
	require.NoError(t, err)
	assert.Equal(t, initial.Fingerprint, fresh.Fingerprint, "scratch schemas must not affect the identity")
	report, err := diffplan.PlanWithRowSecurity(t.Context(), pool, schema, desired)
	require.ErrorIs(t, err, schemadiff.ErrUnsupportedChange)
	assert.Empty(t, report.Statements, "matching a review never enables execution")
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`CREATE POLICY other_reader ON %s.documents
     FOR SELECT USING (owner_id = 9);`, schema))
	require.NoError(t, err)
	before, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	fresh, err = diffplan.VerifyRowSecurityReview(t.Context(), pool, schema, desired, initial.Fingerprint)
	require.ErrorIs(t, err, schemadiff.ErrStaleRowSecurityReview)
	assert.NotEqual(t, initial.Fingerprint, fresh.Fingerprint)
	after, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

func TestReviewFingerprintRechecksDesiredPolicy(t *testing.T) {
	pool, schema := rowSecurityTable(t)
	initial := reviewDesired(t, pool, schema, `CREATE TABLE documents (
     id bigint PRIMARY KEY,
     owner_id bigint NOT NULL
 );
 ALTER TABLE documents ENABLE ROW LEVEL SECURITY;
 CREATE POLICY readers ON documents FOR SELECT USING (owner_id = 7);`)
	desired, err := statement.ParseDesiredWithRowSecurity(`CREATE TABLE documents (
     id bigint PRIMARY KEY,
     owner_id bigint NOT NULL
 );
 ALTER TABLE documents ENABLE ROW LEVEL SECURITY;
 CREATE POLICY readers ON documents FOR SELECT USING (owner_id = 9);`)
	require.NoError(t, err)
	_, err = diffplan.VerifyRowSecurityReview(t.Context(), pool, schema, desired, initial.Fingerprint)
	require.ErrorIs(t, err, schemadiff.ErrStaleRowSecurityReview)
}

func TestReviewFingerprintRejectsConvergedLiveDefinition(t *testing.T) {
	pool, schema := rowSecurityTable(t)
	sql := `CREATE TABLE documents (
     id bigint PRIMARY KEY,
     owner_id bigint NOT NULL
 );
 ALTER TABLE documents ENABLE ROW LEVEL SECURITY;`
	initial := reviewDesired(t, pool, schema, sql)
	desired, err := statement.ParseDesiredWithRowSecurity(sql)
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`ALTER TABLE %s.documents ENABLE ROW LEVEL SECURITY;`, schema))
	require.NoError(t, err)
	fresh, err := diffplan.VerifyRowSecurityReview(t.Context(), pool, schema, desired, initial.Fingerprint)
	require.ErrorIs(t, err, schemadiff.ErrStaleRowSecurityReview)
	assert.Empty(t, fresh.Changes, "convergence must not bypass stale review detection")
}

func TestReviewFingerprintRejectsMissingLiveTable(t *testing.T) {
	pool, schema := rowSecurityTable(t)
	sql := `CREATE TABLE documents (
     id bigint PRIMARY KEY,
     owner_id bigint NOT NULL
 );
 ALTER TABLE documents ENABLE ROW LEVEL SECURITY;`
	initial := reviewDesired(t, pool, schema, sql)
	desired, err := statement.ParseDesiredWithRowSecurity(sql)
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`DROP TABLE %s.documents;`, schema))
	require.NoError(t, err)
	review, err := diffplan.VerifyRowSecurityReview(t.Context(), pool, schema, desired, initial.Fingerprint)
	require.ErrorIs(t, err, schemadiff.ErrTableNotFound)
	assert.Empty(t, review.Fingerprint, "failed inspection cannot issue a fresh review")
}
