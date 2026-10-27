package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/schemachange"
	"github.com/block/pg-sprite/pkg/verdict"
)

// copyStartedDeadline bounds the wait for the copy to land its first rows
// in the shadow; the table is large enough that the copy is still running
// long after that.
const copyStartedDeadline = 30 * time.Second

// An operator's first signal while the copy is running stops the run
// through its own stop path and reports the cancellation as the caller's:
// the copier returns the context's cause, and the cause must still read
// as a cancellation, or the verdict says the run broke.
func TestMigrateSignalDuringCopyReportsCancelledByCaller(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %s.t (
			id int PRIMARY KEY,
			v  text
		)`, schema))
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		INSERT INTO %s.t
		SELECT g, repeat('x', 200)
		FROM generate_series(1, 2000000) g`, schema))
	require.NoError(t, err)
	shadow := pgx.Identifier{schema, schemachange.ShadowName(schema, "t")}.Sanitize()

	sigs := make(chan os.Signal, 2)
	ctx, stop := twoStage(t.Context(), sigs, func(code int) { t.Errorf("the process ended with %d", code) })
	defer stop()
	signalled := make(chan struct{})
	go func() {
		defer close(signalled)
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		deadline := time.After(copyStartedDeadline)
		for {
			// The query fails until the shadow exists; that is the wait.
			var copying bool
			err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+shadow+`)`).Scan(&copying)
			if err == nil && copying {
				sigs <- os.Interrupt
				return
			}
			select {
			case <-ticker.C:
			case <-deadline:
				return
			case <-ctx.Done():
				return
			}
		}
	}()

	cmd := newMigrateCmd(url, fmt.Sprintf("ALTER TABLE %s.t ALTER COLUMN id TYPE bigint", schema))
	cmd.FreeDisk = byteSize(testutil.UnlimitedDisk)
	cmd.Quiesced = schema + ".t"
	cmd.JSON = true
	var out strings.Builder
	err = cmd.run(ctx, &out)
	<-signalled
	require.Error(t, err)
	var v verdict.Verdict
	require.NoError(t, json.Unmarshal([]byte(out.String()), &v))
	assert.Equal(t, verdict.OutcomeFailed, v.Outcome)
	assert.Equal(t, "cancelled-by-caller", v.Code)
}
