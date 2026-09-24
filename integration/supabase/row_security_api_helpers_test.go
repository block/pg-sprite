package supabase_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func rlsAPITable(t *testing.T, name string) *pgxpool.Pool {
	t.Helper()
	pool := fixture(t)
	table := newTable(t, pool, name)
	// Grant table access to both roles so refusals prove RLS, not missing grants.
	execSQL(t, pool, "GRANT SELECT, INSERT, UPDATE, DELETE ON "+table+" TO authenticated, anon")
	execSQL(t, pool, "INSERT INTO "+table+` VALUES
     (1, '00000000-0000-0000-0000-000000000001', 'first'),
     (2, '00000000-0000-0000-0000-000000000002', 'second')`)
	execSQL(t, pool, "NOTIFY pgrst, 'reload schema'")
	bearer := token(t, 1)
	const cacheDeadline = 30 * time.Second
	const cachePoll = 200 * time.Millisecond
	require.Eventually(t, func() bool {
		_, err := get(t.Context(), "http://127.0.0.1:55439/"+name+"?select=id", bearer)
		return err == nil
	}, cacheDeadline, cachePoll, "PostgREST must discover the fixture table")
	return pool
}

func applyAPIRLS(t *testing.T, pool *pgxpool.Pool, sql string) executor.RowSecurityReport {
	t.Helper()
	desired, err := statement.ParseDesiredWithRowSecurity(sql)
	require.NoError(t, err)
	report, err := executor.ExecuteRowSecurity(t.Context(), pool, "public", desired, executor.Budget{LockTimeout: 100 * time.Millisecond, StatementTimeout: 5 * time.Second})
	require.NoError(t, err)
	return report
}

func rlsRequest(t *testing.T, method, path string, tenant int, payload string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, "http://127.0.0.1:55439/"+path, bytes.NewBufferString(payload))
	require.NoError(t, err)
	if tenant != 0 {
		req.Header.Set("Authorization", "Bearer "+token(t, tenant))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Prefer", "return=representation")
	client := http.Client{Timeout: 2 * time.Second}
	response, err := client.Do(req)
	require.NoError(t, err)
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	closeErr := response.Body.Close()
	require.NoError(t, readErr)
	require.NoError(t, closeErr)
	return response.StatusCode, body
}

func assertRLSRows(t *testing.T, status int, body []byte, wantStatus int, ids ...int) {
	t.Helper()
	require.Equal(t, wantStatus, status, "%s", body)
	var rows []struct {
		ID int `json:"id"`
	}
	require.NoError(t, json.Unmarshal(body, &rows))
	actual := make([]int, 0, len(rows))
	for _, row := range rows {
		actual = append(actual, row.ID)
	}
	assert.Equal(t, append([]int{}, ids...), actual)
}

func assertRLSRead(t *testing.T, name string, tenant int, ids ...int) {
	t.Helper()
	status, body := rlsRequest(t, http.MethodGet, name+"?select=id&order=id", tenant, "")
	assertRLSRows(t, status, body, http.StatusOK, ids...)
}

func assertRLSInsertDenied(t *testing.T, name string, tenant, id, owner int) {
	t.Helper()
	status, body := rlsRequest(t, http.MethodPost, name, tenant, fmt.Sprintf(`{"id":%d,"owner_id":%q,"body":"denied"}`, id, tenantID(owner)))
	wantStatus := http.StatusForbidden
	if tenant == 0 {
		wantStatus = http.StatusUnauthorized
	}
	assertRLSDenied(t, status, body, wantStatus)
}

func assertRLSDenied(t *testing.T, status int, body []byte, wantStatus int) {
	t.Helper()
	require.Equal(t, wantStatus, status, "%s", body)
	var result struct {
		Code string `json:"code"`
	}
	require.NoError(t, json.Unmarshal(body, &result))
	assert.Equal(t, "42501", result.Code)
}
