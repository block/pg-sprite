package testutil_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
)

// The replication pair holds what its callers rely on: the settings reach
// both servers, the standby is in recovery, and it streams through the
// physical slot on the primary, so the primary keeps what the standby
// still needs.
func TestStartPrimaryWithStandbyAppliesSettingsToBothServers(t *testing.T) {
	pair := testutil.StartPrimaryWithStandby(t, "work_mem=7MB")

	primary := connect(t, pair.PrimaryURL)
	standby := connect(t, pair.StandbyURL)
	for name, conn := range map[string]*pgx.Conn{"primary": primary, "standby": standby} {
		var workMem string
		require.NoError(t, conn.QueryRow(t.Context(), `SHOW work_mem`).Scan(&workMem))
		assert.Equal(t, "7MB", workMem, "the setting reaches the %s", name)
	}

	var inRecovery bool
	require.NoError(t, standby.QueryRow(t.Context(), `SELECT pg_catalog.pg_is_in_recovery()`).Scan(&inRecovery))
	assert.True(t, inRecovery, "the standby is in recovery")

	var slotType string
	var active bool
	require.NoError(t, primary.QueryRow(t.Context(), `
		SELECT slot_type, active
		FROM pg_catalog.pg_replication_slots
		WHERE slot_name = $1`, testutil.StandbySlotName).Scan(&slotType, &active))
	assert.Equal(t, "physical", slotType)
	assert.True(t, active, "the standby streams through the primary's slot")
}

func connect(t *testing.T, url string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), url)
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, conn.Close(context.WithoutCancel(t.Context())))
	})
	return conn
}
