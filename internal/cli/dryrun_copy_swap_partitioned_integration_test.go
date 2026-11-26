package cli

import (
	"encoding/json"
	"fmt"
	neturl "net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/verdict"
)

// requireDryRunRefusesLikeApply runs alter as migrate --dry-run and as an
// apply that opens copy-and-swap and gives it free disk, so the table's
// shape is what refuses it, and requires both to end in the same refusal
// verdict, exit code included. It returns the dry run's verdict.
func requireDryRunRefusesLikeApply(t *testing.T, url, alter string) verdict.Verdict {
	t.Helper()
	run := func(cmd *MigrateCmd) verdict.Verdict {
		cmd.JSON = true
		var out strings.Builder
		require.ErrorIs(t, cmd.run(t.Context(), &out), verdict.ErrRefused,
			"the refusal exit code must not depend on --dry-run")
		var v verdict.Verdict
		require.NoError(t, json.Unmarshal([]byte(out.String()), &v))
		return v
	}
	dry := newMigrateCmd(url, alter)
	dry.DryRun = true
	dryRun := run(dry)
	apply := newMigrateCmd(url, alter)
	apply.CopyAndSwap = true
	apply.FreeDisk = byteSize(testutil.UnlimitedDisk)
	applied := run(apply)

	assert.Equal(t, verdict.OutcomeRefused, dryRun.Outcome)
	assert.Equal(t, verdict.ReasonCopyAndSwapRefused, dryRun.Reason)
	assert.Equal(t, verdict.ClassCapabilityBoundary, dryRun.Class)
	assert.Equal(t, applied, dryRun, "the dry run must report the refusal the apply reports")
	return dryRun
}

// A column retype that rewrites the table routes to copy-and-swap, whose
// preflight refuses a partitioned parent before it builds anything: the
// shadow table would be a plain table and the rename swap would strand the
// partitions. The dry run must end in that same refusal, with the apply's
// whole verdict, instead of an executable plan.
func TestMigrateDryRunRefusesCopySwapOnPartitionedParentLikeApply(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.events (
			id bigint NOT NULL,
			created date NOT NULL,
			note text,
			PRIMARY KEY (id, created)
		) PARTITION BY RANGE (created);
		CREATE TABLE %[1]s.events_2026 PARTITION OF %[1]s.events
			FOR VALUES FROM ('2026-01-01') TO ('2027-01-01')`, schema))
	require.NoError(t, err)

	dryRun := requireDryRunRefusesLikeApply(t, url,
		fmt.Sprintf("ALTER TABLE %s.events ALTER COLUMN note TYPE varchar(10)", schema))
	assert.Equal(t, verdict.Cause(preflight.CopySwapCausePartitioned), dryRun.Cause)
}

// A partition and both ends of an inheritance tree are refused by the
// copy-and-swap preflight as a partitioned parent is: the swap would detach
// the partition from its parent, or a parent from its children, or a child
// from its parent. The dry run ends in the apply's refusal for each, and for
// an unqualified table it resolves on the session's search_path, as the
// apply does.
func TestMigrateDryRunRefusesCopySwapOnTreeMembersLikeApply(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.events (
			id bigint NOT NULL,
			created date NOT NULL,
			note text,
			PRIMARY KEY (id, created)
		) PARTITION BY RANGE (created);
		CREATE TABLE %[1]s.events_2026 PARTITION OF %[1]s.events
			FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');
		CREATE TABLE %[1]s.base (
			id bigint PRIMARY KEY,
			note text
		);
		CREATE TABLE %[1]s.derived (
			extra text
		) INHERITS (%[1]s.base)`, schema))
	require.NoError(t, err)

	for _, table := range []string{"events_2026", "base", "derived"} {
		t.Run(table, func(t *testing.T) {
			dryRun := requireDryRunRefusesLikeApply(t, url,
				fmt.Sprintf("ALTER TABLE %s.%s ALTER COLUMN note TYPE varchar(10)", schema, table))
			assert.Equal(t, verdict.Cause(preflight.CopySwapCausePartitioned), dryRun.Cause)
		})
	}
	t.Run("unqualified partition", func(t *testing.T) {
		u, err := neturl.Parse(url)
		require.NoError(t, err)
		q := u.Query()
		q.Set("options", "-csearch_path="+schema)
		u.RawQuery = q.Encode()
		dryRun := requireDryRunRefusesLikeApply(t, u.String(),
			"ALTER TABLE events_2026 ALTER COLUMN note TYPE varchar(10)")
		assert.Equal(t, verdict.Cause(preflight.CopySwapCausePartitioned), dryRun.Cause)
		assert.Equal(t, schema+".events_2026", dryRun.Table, "the verdict names the table the search_path resolves")
	})
}

// A partitioned parent with FORCE ROW LEVEL SECURITY is refused by the
// copy-and-swap preflight for the forced row security, which it decides
// before the partitioning: the owner-run copier would be subject to the
// table's policies. The dry run ends in that same refusal, not a plan.
func TestMigrateDryRunRefusesCopySwapOnAForceRLSPartitionedParentLikeApply(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.events (
			id bigint NOT NULL,
			created date NOT NULL,
			note text,
			PRIMARY KEY (id, created)
		) PARTITION BY RANGE (created);
		CREATE TABLE %[1]s.events_2026 PARTITION OF %[1]s.events
			FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');
		ALTER TABLE %[1]s.events ENABLE ROW LEVEL SECURITY;
		ALTER TABLE %[1]s.events FORCE ROW LEVEL SECURITY`, schema))
	require.NoError(t, err)

	dryRun := requireDryRunRefusesLikeApply(t, url,
		fmt.Sprintf("ALTER TABLE %s.events ALTER COLUMN note TYPE varchar(10)", schema))
	assert.Equal(t, verdict.Cause(preflight.CopySwapCauseForceRLS), dryRun.Cause)
}
