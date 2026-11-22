package schemadiff_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
)

// A table that gained its unnamed checks through a history of ALTERs holds
// them under the server's numbering in the order they were added. The same
// table declared in one file numbers them in file order, so the names
// differ while every definition matches: the plan is empty. A check the
// file names keeps its stated name and is matched by it.
func TestDiffIgnoresTheNumberingOfUnnamedChecks(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)

	for _, sql := range []string{
		`CREATE TABLE %[1]s.requests (
			id bigint PRIMARY KEY,
			stage text NOT NULL,
			frozen_at timestamptz,
			digest text,
			aborted_at timestamptz,
			aborted_by text,
			CONSTRAINT stage_known CHECK (stage IN ('new', 'done', 'aborted')),
			CHECK ((stage = 'aborted') = (aborted_at IS NOT NULL))
		)`,
		`ALTER TABLE %[1]s.requests ADD CHECK ((frozen_at IS NULL) = (digest IS NULL))`,
		`ALTER TABLE %[1]s.requests ADD CHECK ((aborted_by IS NULL) OR (aborted_at IS NOT NULL))`,
	} {
		_, err := pool.Exec(t.Context(), fmt.Sprintf(sql, schema))
		require.NoError(t, err)
	}

	ds, err := statement.ParseDesired(`
CREATE TABLE requests (
  id bigint PRIMARY KEY,
  stage text NOT NULL,
  frozen_at timestamptz,
  digest text,
  aborted_at timestamptz,
  aborted_by text,
  CONSTRAINT stage_known CHECK (stage IN ('new', 'done', 'aborted')),
  CHECK ((stage = 'aborted') = (aborted_at IS NOT NULL)),
  CHECK ((aborted_by IS NULL) OR (aborted_at IS NOT NULL)),
  CHECK ((frozen_at IS NULL) = (digest IS NULL))
);
`)
	require.NoError(t, err)

	live, err := schemadiff.Introspect(t.Context(), pool, schema, "requests")
	require.NoError(t, err)
	desired, err := schemadiff.IntrospectDesired(t.Context(), pool, schema, ds)
	require.NoError(t, err)

	byName := func(m schemadiff.Model) map[string]string {
		defs := make(map[string]string)
		for _, c := range m.Constraints {
			defs[c.Name] = c.Def
		}
		return defs
	}
	// The history and the file number the last two checks the other way
	// round, which is what a name-keyed diff would read as four changes.
	require.Equal(t, byName(live)["requests_check1"], byName(desired)["requests_check2"])
	require.Equal(t, byName(live)["requests_check2"], byName(desired)["requests_check1"])

	generated := make(map[string]bool)
	for _, c := range desired.Constraints {
		generated[c.Name] = c.GeneratedName
	}
	assert.Equal(t, map[string]bool{
		"requests_pkey":   false,
		"stage_known":     false,
		"requests_check":  true,
		"requests_check1": true,
		"requests_check2": true,
	}, generated)

	changes, err := schemadiff.Diff(schema, live, desired)
	require.NoError(t, err)
	assert.Empty(t, changes)
}
