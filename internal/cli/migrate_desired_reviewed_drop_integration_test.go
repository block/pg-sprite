package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/verdict"
)

const reviewedDropDesired = `CREATE TABLE documents (
 id int PRIMARY KEY,
 owner_id int NOT NULL
 );`

// An operator reviews a desired file that removes a column and an index. The
// diff prints the plan's fingerprint; a dry run and a run without the pin
// and acknowledgement refuse, naming both; a dry run with a stale pin refuses
// as the run does; a dry run with the right flags previews the plan the run
// executes; the run with them drops the column and the index (concurrently),
// converges the table, and a repeat run plans nothing.
func TestMigrateDesiredAppliesAReviewedDrop(t *testing.T) {
	url, schema, pool := desiredFixture(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`ALTER TABLE %[1]s.documents ADD COLUMN legacy text;
CREATE INDEX documents_owner_idx ON %[1]s.documents (owner_id)`, schema))
	require.NoError(t, err)
	before, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)

	cmd := desiredCommand(t, url, schema, reviewedDropDesired)
	diff := &DiffCmd{DBFlags: DBFlags{URL: url}, Schema: schema, Desired: cmd.Desired}
	var out strings.Builder
	require.NoError(t, diff.run(t.Context(), &out))
	diffText := out.String()
	diff.JSON = true
	out.Reset()
	require.NoError(t, diff.run(t.Context(), &out))
	var reviewed plan.Report
	require.NoError(t, json.Unmarshal([]byte(out.String()), &reviewed))
	require.Len(t, reviewed.Statements, 2)
	assert.Contains(t, diffText, "fingerprint:\n  "+reviewed.Fingerprint+"\n")
	table := schema + ".documents"
	flags := []string{"--expect-fingerprint", reviewed.Fingerprint, "--allow-destructive", table}
	unchanged := func(t *testing.T) {
		t.Helper()
		after, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
		require.NoError(t, err)
		assert.Equal(t, before, after, "nothing may have run")
	}

	t.Run("a dry run without the flags refuses and names them", func(t *testing.T) {
		dry := desiredCommand(t, url, schema, reviewedDropDesired, "--dry-run")
		dry.JSON = false
		var out strings.Builder
		require.ErrorIs(t, dry.run(t.Context(), &out), verdict.ErrRefused)
		assert.Contains(t, out.String(), "--expect-fingerprint "+reviewed.Fingerprint+" --allow-destructive "+table+"\n")
		unchanged(t)
	})

	t.Run("a dry run with a stale pin refuses as the run does", func(t *testing.T) {
		dry := desiredCommand(t, url, schema, reviewedDropDesired,
			"--dry-run", "--expect-fingerprint", "sha256:stale", "--allow-destructive", table)
		var out strings.Builder
		require.ErrorIs(t, dry.run(t.Context(), &out), verdict.ErrRefused)
		var result migrate.DesiredResult
		require.NoError(t, json.Unmarshal([]byte(out.String()), &result))
		assert.Equal(t, verdict.ReasonPlanFingerprintMismatch, result.Reason)
		assert.Contains(t, result.Detail, reviewed.Fingerprint)
		unchanged(t)
	})

	t.Run("a dry run with the flags previews the plan the run executes", func(t *testing.T) {
		dry := desiredCommand(t, url, schema, reviewedDropDesired, append([]string{"--dry-run"}, flags...)...)
		var out strings.Builder
		require.NoError(t, dry.run(t.Context(), &out))
		var preview plan.Report
		require.NoError(t, json.Unmarshal([]byte(out.String()), &preview))
		assert.Equal(t, router.DispositionExecute, preview.Disposition)
		assert.Equal(t, reviewed.Fingerprint, preview.Fingerprint)
		assert.Equal(t, reviewed.Statements, preview.Statements)
		unchanged(t)
	})

	t.Run("a run without the flags refuses", func(t *testing.T) {
		var out strings.Builder
		require.ErrorIs(t, cmd.run(t.Context(), &out), verdict.ErrRefused)
		var result migrate.DesiredResult
		require.NoError(t, json.Unmarshal([]byte(out.String()), &result))
		assert.Equal(t, verdict.ReasonDestructiveChange, result.Reason)
		assert.Empty(t, result.Verdicts)
		assert.Contains(t, result.Detail, "--expect-fingerprint "+reviewed.Fingerprint)
		assert.Contains(t, result.Detail, "--allow-destructive "+table)
		unchanged(t)
	})

	t.Run("a run with the flags drops the column and the index", func(t *testing.T) {
		run := desiredCommand(t, url, schema, reviewedDropDesired, flags...)
		var out strings.Builder
		require.NoError(t, run.run(t.Context(), &out))
		var result migrate.DesiredResult
		require.NoError(t, json.Unmarshal([]byte(out.String()), &result))
		require.Equal(t, verdict.OutcomeExecuted, result.Outcome, result.Detail)
		require.Len(t, result.Verdicts, 2)
		for _, v := range result.Verdicts {
			assert.True(t, v.AllowedDestructive, "statement %q", v.Statement)
		}
		after, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
		require.NoError(t, err)
		require.Len(t, after.Columns, 2)
		require.Len(t, before.Indexes, 1)
		assert.Empty(t, after.Indexes, "the index the file lacks is gone; the primary key is a constraint")

		out.Reset()
		require.NoError(t, run.run(t.Context(), &out))
		var again migrate.DesiredResult
		require.NoError(t, json.Unmarshal([]byte(out.String()), &again))
		assert.Equal(t, verdict.OutcomeExecuted, again.Outcome)
		assert.Empty(t, again.Plan.Statements, "a converged table plans nothing")
	})
}

// The flags apply only where they mean something: an enum type file adds
// labels and never drops one, and a row security change runs in one
// transaction with no plan to pin. Each refuses the flag with an error and
// changes nothing.
func TestMigrateDesiredRejectsThePinWhereItDoesNotApply(t *testing.T) {
	url, schema, pool := desiredFixture(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`CREATE TYPE %s.order_status AS ENUM ('new')`, schema))
	require.NoError(t, err)
	table := schema + ".documents"

	t.Run("an enum type file takes no destructive acknowledgement", func(t *testing.T) {
		cmd := desiredCommand(t, url, schema, "CREATE TYPE order_status AS ENUM ('new', 'paid');\n",
			"--expect-fingerprint", "sha256:r", "--allow-destructive", schema+".order_status")
		var out strings.Builder
		err := cmd.run(t.Context(), &out)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--allow-destructive does not apply to an enum type file")
		live, err := schemadiff.IntrospectEnumType(t.Context(), pool, schema, "order_status")
		require.NoError(t, err)
		assert.Equal(t, []string{"new"}, live.Labels)
	})

	const withPolicy = `CREATE TABLE documents (
 id int PRIMARY KEY,
 owner_id int NOT NULL
 );
 ALTER TABLE documents ENABLE ROW LEVEL SECURITY;
 CREATE POLICY readers ON documents FOR SELECT USING (owner_id = 7);`
	for _, dryRun := range []bool{true, false} {
		t.Run(fmt.Sprintf("a row security change takes no pin, dry run %v", dryRun), func(t *testing.T) {
			cmd := desiredCommand(t, url, schema, withPolicy, "--expect-fingerprint", "sha256:r")
			cmd.DryRun = dryRun
			var out strings.Builder
			err := cmd.run(t.Context(), &out)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "--expect-fingerprint does not apply to this file")
			after, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
			require.NoError(t, err)
			assert.False(t, after.RowSecurity.Enabled, "nothing may have run on %s", table)
		})
	}
}

// A row security file whose policies already match and whose table drops a
// column runs as a reviewed drop. Once it has converged, running the same
// reviewed command again, or its dry run, finds nothing to do and succeeds
// instead of rejecting the flags.
func TestMigrateDesiredRowSecurityFileRerunsAConvergedReviewedDrop(t *testing.T) {
	url, schema, pool := desiredFixture(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`ALTER TABLE %[1]s.documents ADD COLUMN legacy text;
ALTER TABLE %[1]s.documents ENABLE ROW LEVEL SECURITY;
CREATE POLICY readers ON %[1]s.documents FOR SELECT USING (owner_id = 7)`, schema))
	require.NoError(t, err)
	const withPolicy = `CREATE TABLE documents (
 id int PRIMARY KEY,
 owner_id int NOT NULL
 );
 ALTER TABLE documents ENABLE ROW LEVEL SECURITY;
 CREATE POLICY readers ON documents FOR SELECT USING (owner_id = 7);`

	cmd := desiredCommand(t, url, schema, withPolicy)
	diff := &DiffCmd{DBFlags: DBFlags{URL: url}, Schema: schema, Desired: cmd.Desired, JSON: true}
	var out strings.Builder
	require.NoError(t, diff.run(t.Context(), &out))
	var reviewed plan.Report
	require.NoError(t, json.Unmarshal([]byte(out.String()), &reviewed))
	require.Len(t, reviewed.Statements, 1)
	flags := []string{"--expect-fingerprint", reviewed.Fingerprint, "--allow-destructive", schema + ".documents"}

	out.Reset()
	require.NoError(t, desiredCommand(t, url, schema, withPolicy, flags...).run(t.Context(), &out), out.String())
	after, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	require.Len(t, after.Columns, 2, "the reviewed drop ran")

	for _, dryRun := range []bool{false, true} {
		t.Run(fmt.Sprintf("dry run %v", dryRun), func(t *testing.T) {
			again := desiredCommand(t, url, schema, withPolicy, flags...)
			again.DryRun = dryRun
			var out strings.Builder
			require.NoError(t, again.run(t.Context(), &out), out.String())
		})
	}
}
