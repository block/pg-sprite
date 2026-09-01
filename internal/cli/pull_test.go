package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/verdict"
)

func TestPullTablesContinuesAfterFailures(t *testing.T) {
	wantErr := errors.New("database read failed")
	called := make([]string, 0, 3)
	pull := func(_ context.Context, _ *pgxpool.Pool, _, table, _ string) error {
		called = append(called, table)
		switch table {
		case "bad_render":
			return &renderRefusal{err: errors.New("unsupported table")}
		case "bad_read":
			return wantErr
		default:
			return nil
		}
	}

	results := pullTables(t.Context(), nil, "public", t.TempDir(),
		[]string{"bad_render", "good", "bad_read"}, pull)

	assert.Equal(t, []string{"bad_render", "good", "bad_read"}, called)
	require.Len(t, results, 3)
	assert.Equal(t, pullStatusRefused, results[0].status)
	assert.Equal(t, pullStatusPulled, results[1].status)
	assert.Equal(t, pullStatusError, results[2].status)
	assert.ErrorIs(t, pullResultsError(results), ErrPullFailed)
}

func TestPullTablesReportsCaseFoldedPathCollision(t *testing.T) {
	var called []string
	pull := func(_ context.Context, _ *pgxpool.Pool, _, table, _ string) error {
		called = append(called, table)
		return nil
	}

	results := pullTables(t.Context(), nil, "public", t.TempDir(), []string{"Accounts", "accounts"}, pull)

	assert.Equal(t, []string{"Accounts"}, called)
	require.Len(t, results, 2)
	assert.Equal(t, pullStatusPulled, results[0].status)
	assert.Equal(t, pullStatusError, results[1].status)
	assert.ErrorContains(t, results[1].err, `case collision with table "Accounts"`)
}

func TestPullTablesStopsAfterContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	pull := func(_ context.Context, _ *pgxpool.Pool, _, _, _ string) error {
		cancel()
		return nil
	}

	results := pullTables(ctx, nil, "public", t.TempDir(), []string{"first", "second", "third"}, pull)

	require.Len(t, results, 2)
	assert.Equal(t, pullStatusPulled, results[0].status)
	assert.Equal(t, "second", results[1].table)
	assert.ErrorIs(t, results[1].err, context.Canceled)
}

func TestPullResultsErrorReturnsRefusalWhenNoOperationalErrors(t *testing.T) {
	results := []pullResult{{status: pullStatusPulled}, {status: pullStatusRefused}}
	assert.ErrorIs(t, pullResultsError(results), verdict.ErrRefused)
}

func TestTableOutputPathRejectsPathSeparators(t *testing.T) {
	_, err := tableOutputPath(t.TempDir(), "../outside")
	require.Error(t, err)
}

func TestWritePullTextSummarizesOutcomes(t *testing.T) {
	results := []pullResult{
		{table: "accounts", path: "schema/accounts.sql", status: pullStatusPulled},
		{table: "metrics", status: pullStatusRefused, err: errors.New("partitioned")},
		{table: "events", status: pullStatusError, err: errors.New("exists")},
	}
	var out strings.Builder
	require.NoError(t, writePullText(&out, results))
	assert.Equal(t, "PULLED  accounts -> schema/accounts.sql\n"+
		"REFUSED metrics: partitioned\n"+
		"ERROR   events: exists\n"+
		"Summary: 1 pulled, 1 refused, 1 errors\n", out.String())
}

func TestWritePullTextRejectsUnknownStatus(t *testing.T) {
	var out strings.Builder
	err := writePullText(&out, []pullResult{{table: "events", status: pullStatus("future")}})
	require.ErrorContains(t, err, `unexpected pull status "future"`)
	assert.Empty(t, out.String())
}

func TestPullOneTableDoesNotOverwriteExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.sql")
	require.NoError(t, os.WriteFile(path, []byte("keep me"), 0o600))
	err := pullRenderedFile(path, "replacement")
	require.Error(t, err)
	got, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, "keep me", string(got))
}
