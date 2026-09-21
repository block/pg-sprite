package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiffRechecksRowSecurityFingerprint(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`CREATE TABLE %s.documents (
     id bigint PRIMARY KEY
 );`, schema))
	require.NoError(t, err)
	file := filepath.Join(t.TempDir(), "documents.sql")
	require.NoError(t, os.WriteFile(file, []byte(`CREATE TABLE documents (
     id bigint PRIMARY KEY
 );
 ALTER TABLE documents ENABLE ROW LEVEL SECURITY;`), 0600))
	cmd := &DiffCmd{DBFlags: DBFlags{URL: url}, Schema: schema, Desired: file, JSON: true}
	var out strings.Builder
	require.ErrorIs(t, cmd.run(t.Context(), &out), verdict.ErrRefused)
	var result struct {
		Outcome verdict.Outcome              `json:"outcome"`
		Review  schemadiff.RowSecurityReview `json:"row_security_review"`
		Matches *bool                        `json:"review_matches"`
	}
	require.NoError(t, json.Unmarshal([]byte(out.String()), &result))
	assert.Nil(t, result.Matches)
	cmd.ExpectRLSReview = result.Review.Fingerprint
	out.Reset()
	require.ErrorIs(t, cmd.run(t.Context(), &out), verdict.ErrRefused)
	require.NoError(t, json.Unmarshal([]byte(out.String()), &result))
	require.NotNil(t, result.Matches)
	assert.True(t, *result.Matches)
	assert.Equal(t, verdict.OutcomeRefused, result.Outcome)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`ALTER TABLE %s.documents FORCE ROW LEVEL SECURITY;`, schema))
	require.NoError(t, err)
	out.Reset()
	err = cmd.run(t.Context(), &out)
	require.ErrorIs(t, err, verdict.ErrRefused)
	require.ErrorIs(t, err, schemadiff.ErrStaleRowSecurityReview)
	require.NoError(t, json.Unmarshal([]byte(out.String()), &result))
	require.NotNil(t, result.Matches)
	assert.False(t, *result.Matches)
	assert.NotEqual(t, cmd.ExpectRLSReview, result.Review.Fingerprint)
	cmd.JSON, cmd.SQL = false, true
	out.Reset()
	require.ErrorIs(t, cmd.run(t.Context(), &out), schemadiff.ErrStaleRowSecurityReview)
	_, err = statement.ParseDesired(out.String())
	assert.ErrorIs(t, err, statement.ErrEmptyDesired, "verification must emit no executable SQL")
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`DROP TABLE %s.documents;`, schema))
	require.NoError(t, err)
	cmd.JSON, cmd.SQL = true, false
	out.Reset()
	err = cmd.run(t.Context(), &out)
	require.ErrorIs(t, err, verdict.ErrRefused)
	require.ErrorIs(t, err, schemadiff.ErrTableNotFound)
	var missing map[string]any
	require.NoError(t, json.Unmarshal([]byte(out.String()), &missing))
	assert.Equal(t, "refused", missing["outcome"])
	assert.NotContains(t, missing, "review_matches")
	assert.NotContains(t, missing, "row_security_review")
}

func TestDiffFingerprintRequiresExplicitRLSScope(t *testing.T) {
	file := filepath.Join(t.TempDir(), "documents.sql")
	require.NoError(t, os.WriteFile(file, []byte(`CREATE TABLE documents (
     id bigint PRIMARY KEY
 );`), 0600))
	cmd := &DiffCmd{Desired: file, ExpectRLSReview: "rls-review-v1:" + strings.Repeat("a", 64)}
	var out strings.Builder
	require.Error(t, cmd.run(t.Context(), &out))
	assert.Empty(t, out.String(), "cannot silently downgrade to a table-only comparison")
}
