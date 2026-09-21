package schemadiff_test

import (
	"fmt"
	"testing"

	"github.com/block/pg-sprite/pkg/diffplan"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func reviewDesired(t *testing.T, pool *pgxpool.Pool, schema, sql string) schemadiff.RowSecurityReview {
	t.Helper()
	before, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	desired, err := statement.ParseDesiredWithRowSecurity(sql)
	require.NoError(t, err)
	report, err := diffplan.PlanWithRowSecurity(t.Context(), pool, schema, desired)
	require.ErrorIs(t, err, schemadiff.ErrUnsupportedChange)
	assert.Empty(t, report.Statements)
	var review *diffplan.RowSecurityReviewRequired
	require.ErrorAs(t, err, &review)
	after, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	assert.Equal(t, before, after, "review must not modify live access rules")
	return review.Review
}

func TestReviewDisablingRLSMayWidenAccess(t *testing.T) {
	pool, schema := rowSecurityTable(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`ALTER TABLE %s.documents ENABLE ROW LEVEL SECURITY`, schema))
	require.NoError(t, err)
	r := reviewDesired(t, pool, schema, `CREATE TABLE documents (
 id bigint PRIMARY KEY,
 owner_id bigint NOT NULL
 );
 ALTER TABLE documents DISABLE ROW LEVEL SECURITY;`)
	require.Len(t, r.Changes, 1)
	assert.Equal(t, schemadiff.SecurityEnabled, r.Changes[0].Kind)
	assert.True(t, *r.Changes[0].BeforeSetting)
	assert.False(t, *r.Changes[0].AfterSetting)
	assert.Equal(t, schemadiff.AccessMayWiden, r.Changes[0].Impact)
	assert.True(t, r.TableComparisonComplete)
	assert.False(t, r.TableChanged)
}

func TestReviewRemovalOfLastRestrictivePolicy(t *testing.T) {
	pool, schema := rowSecurityTable(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`CREATE POLICY readers ON %s.documents
 AS RESTRICTIVE FOR SELECT USING (owner_id = 7)`, schema))
	require.NoError(t, err)
	r := reviewDesired(t, pool, schema, `CREATE TABLE documents (
 id bigint PRIMARY KEY,
 owner_id bigint NOT NULL
 );
 ALTER TABLE documents DISABLE ROW LEVEL SECURITY;`)
	require.Len(t, r.Changes, 1)
	c := r.Changes[0]
	assert.Equal(t, schemadiff.SecurityPolicyRemoved, c.Kind)
	assert.Equal(t, schemadiff.AccessMayWiden, c.Impact)
	require.NotNil(t, c.BeforePolicy)
	assert.Nil(t, c.AfterPolicy)
	assert.Equal(t, "(owner_id = 7)", *c.BeforePolicy.Using)
}

func TestReviewPredicateChangeWithTableChange(t *testing.T) {
	pool, schema := rowSecurityTable(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`CREATE POLICY readers ON %s.documents
 FOR SELECT USING (owner_id = 7)`, schema))
	require.NoError(t, err)
	r := reviewDesired(t, pool, schema, `CREATE TABLE documents (
 id bigint PRIMARY KEY,
 owner_id bigint NOT NULL,
 title text
 );
 ALTER TABLE documents DISABLE ROW LEVEL SECURITY;
 CREATE POLICY readers ON documents FOR SELECT USING (true);`)
	require.Len(t, r.Changes, 1)
	c := r.Changes[0]
	assert.Equal(t, schemadiff.SecurityPolicyChanged, c.Kind)
	assert.Equal(t, schemadiff.AccessReviewRequired, c.Impact)
	assert.Equal(t, "(owner_id = 7)", *c.BeforePolicy.Using)
	assert.Equal(t, "true", *c.AfterPolicy.Using)
	assert.Nil(t, c.AfterPolicy.WithCheck)
	assert.True(t, r.TableChanged)
}

func TestReviewAddedPermissivePolicy(t *testing.T) {
	pool, schema := rowSecurityTable(t)
	r := reviewDesired(t, pool, schema, `CREATE TABLE documents (
 id bigint PRIMARY KEY,
 owner_id bigint NOT NULL
 );
 ALTER TABLE documents DISABLE ROW LEVEL SECURITY;
 CREATE POLICY readers ON documents FOR SELECT USING (owner_id = 7);`)
	require.Len(t, r.Changes, 1)
	assert.Equal(t, schemadiff.SecurityPolicyAdded, r.Changes[0].Kind)
	assert.Equal(t, schemadiff.AccessMayWiden, r.Changes[0].Impact)
	assert.Nil(t, r.Changes[0].BeforePolicy)
	assert.Equal(t, []string{"public"}, r.Changes[0].AfterPolicy.Roles)
}
