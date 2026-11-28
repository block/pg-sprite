package cli

import (
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
)

// A dry run of a change between timestamp and timestamptz names the
// session time zone the change converts each stored value in. In UTC, for
// a column no index reads, PostgreSQL relabels it in place; in any other
// zone every value moves by the zone's offset and the table is rewritten.
func TestMigrateDryRunNamesTheTimeZoneATimestampChangeConvertsIn(t *testing.T) {
	base := testutil.StartPostgres(t)
	for _, tc := range []struct {
		zone, reason string
	}{
		{zone: "UTC", reason: "binary-coercible"},
		{zone: "America/New_York", reason: "type-rewrite"},
	} {
		t.Run(tc.zone, func(t *testing.T) {
			u, err := url.Parse(base)
			require.NoError(t, err)
			q := u.Query()
			q.Set("TimeZone", tc.zone)
			u.RawQuery = q.Encode()
			pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: u.String()})
			require.NoError(t, err)
			t.Cleanup(pool.Close)
			schema := testutil.NewSchema(t, pool)
			_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.events (id bigint PRIMARY KEY, seen_at timestamp)", schema))
			require.NoError(t, err)

			cmd := newMigrateCmd(u.String(), fmt.Sprintf("ALTER TABLE %s.events ALTER COLUMN seen_at TYPE timestamptz", schema))
			cmd.DryRun = true
			// The rewrite routes to copy-and-swap, whose plan the dry run
			// prints only with the route opened.
			cmd.CopyAndSwap = true
			var out strings.Builder
			runErr := cmd.run(t.Context(), &out)
			// The text wraps at the terminal width; compare it unwrapped.
			text := strings.Join(strings.Fields(out.String()), " ")
			assert.Contains(t, text, tc.reason, "run error: %v", runErr)
			assert.Contains(t, text, "converts each stored value in the session time zone "+tc.zone)
		})
	}
}
