package dbconn

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testURL = "postgres://user@localhost:5432/app"

func TestBuildPoolConfigDefaults(t *testing.T) {
	pc, err := buildPoolConfig(Config{URL: testURL})
	require.NoError(t, err)

	rp := pc.ConnConfig.RuntimeParams
	assert.Equal(t, "3000", rp["lock_timeout"])
	assert.Equal(t, "30000", rp["statement_timeout"])
	assert.Equal(t, "pg-sprite", rp["application_name"])
	assert.Equal(t, DefaultConnectTimeout, pc.ConnConfig.ConnectTimeout)

	// Unset knobs keep pgxpool's own defaults rather than zeroing them out.
	assert.Positive(t, pc.MaxConns)
	assert.Positive(t, pc.MaxConnLifetime)
	assert.Positive(t, pc.HealthCheckPeriod)
	assert.Nil(t, pc.BeforeConnect)
	assert.Nil(t, pc.ConnConfig.Tracer)
}

func TestBuildPoolConfigOverrides(t *testing.T) {
	hookCalled := false
	cfg := Config{
		URL:                   testURL,
		LockTimeout:           1500 * time.Millisecond,
		StatementTimeout:      45 * time.Second,
		ConnectTimeout:        7 * time.Second,
		MaxConns:              20,
		MinConns:              2,
		MaxConnLifetime:       30 * time.Minute,
		MaxConnLifetimeJitter: 5 * time.Minute,
		MaxConnIdleTime:       10 * time.Minute,
		HealthCheckPeriod:     30 * time.Second,
		QueryExecMode:         pgx.QueryExecModeExec,
		Logger:                slog.Default(),
		BeforeConnect: func(context.Context, *pgx.ConnConfig) error {
			hookCalled = true
			return nil
		},
	}
	pc, err := buildPoolConfig(cfg)
	require.NoError(t, err)

	rp := pc.ConnConfig.RuntimeParams
	assert.Equal(t, "1500", rp["lock_timeout"])
	assert.Equal(t, "45000", rp["statement_timeout"])
	assert.Equal(t, 7*time.Second, pc.ConnConfig.ConnectTimeout)
	assert.Equal(t, int32(20), pc.MaxConns)
	assert.Equal(t, int32(2), pc.MinConns)
	assert.Equal(t, 30*time.Minute, pc.MaxConnLifetime)
	assert.Equal(t, 5*time.Minute, pc.MaxConnLifetimeJitter)
	assert.Equal(t, 10*time.Minute, pc.MaxConnIdleTime)
	assert.Equal(t, 30*time.Second, pc.HealthCheckPeriod)
	assert.Equal(t, pgx.QueryExecModeExec, pc.ConnConfig.DefaultQueryExecMode)
	assert.NotNil(t, pc.ConnConfig.Tracer, "a Logger must enable statement tracing")

	require.NotNil(t, pc.BeforeConnect)
	require.NoError(t, pc.BeforeConnect(t.Context(), pc.ConnConfig.Copy()))
	assert.True(t, hookCalled, "BeforeConnect must be wired through verbatim")
}

func TestBuildPoolConfigKeepsPgxExecModeDefaultWhenUnset(t *testing.T) {
	pc, err := buildPoolConfig(Config{URL: testURL})
	require.NoError(t, err)
	assert.Equal(t, pgx.QueryExecModeCacheStatement, pc.ConnConfig.DefaultQueryExecMode,
		"unset QueryExecMode keeps pgx's statement-caching default")
}
