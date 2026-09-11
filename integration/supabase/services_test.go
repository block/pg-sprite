// Package supabase_test exercises the disposable compose/supabase.yml services.
package supabase_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const directURL = "postgres://postgres:pgsprite_test_only@127.0.0.1:55438/postgres?sslmode=disable"
const sessionURL = "postgres://postgres.pgsprite:pgsprite_test_only@127.0.0.1:55440/postgres?sslmode=disable"
const transactionURL = "postgres://postgres.pgsprite:pgsprite_test_only@127.0.0.1:55441/postgres?sslmode=disable"

func fixture(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("SUPABASE_SERVICES_TEST") != "1" {
		t.Skip("start compose/supabase.yml and set SUPABASE_SERVICES_TEST=1")
	}
	// Fixed localhost credentials: this fixture must never target a hosted project.
	return connect(t, directURL)
}

func connect(t *testing.T, url string) *pgxpool.Pool {
	t.Helper()
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

func execSQL(t *testing.T, pool *pgxpool.Pool, sql string) {
	t.Helper()
	_, err := pool.Exec(t.Context(), sql)
	require.NoError(t, err)
}

func newTable(t *testing.T, pool *pgxpool.Pool, name string) string {
	t.Helper()
	table := pgx.Identifier{"public", name}.Sanitize()
	execSQL(t, pool, "DROP TABLE IF EXISTS "+table)
	execSQL(t, pool, "CREATE TABLE "+table+" (id int PRIMARY KEY, owner_id uuid NOT NULL, body text NOT NULL)")
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		_, err := pool.Exec(ctx, "DROP TABLE IF EXISTS "+table)
		assert.NoError(t, err)
	})
	execSQL(t, pool, "ALTER TABLE "+table+" ENABLE ROW LEVEL SECURITY")
	execSQL(t, pool, "CREATE POLICY own_rows ON "+table+" TO authenticated USING (owner_id=auth.uid()) WITH CHECK (owner_id=auth.uid())")
	execSQL(t, pool, "GRANT SELECT ON "+table+" TO authenticated")
	return table
}

func change(t *testing.T, pool *pgxpool.Pool, sql string) verdict.Verdict {
	t.Helper()
	st, err := statement.ParseOne(sql)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	v, err := migrate.Run(ctx, pool, st, migrate.DefaultOptions())
	require.NoError(t, err)
	require.Equal(t, verdict.OutcomeExecuted, v.Outcome)
	return v
}

func token(t *testing.T, tenant int) string {
	t.Helper()
	encode := func(v any) string {
		b, err := json.Marshal(v)
		require.NoError(t, err)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	body := encode(map[string]any{"alg": "HS256", "typ": "JWT"}) + "." + encode(map[string]any{"role": "authenticated", "sub": tenantID(tenant), "exp": time.Now().Add(5 * time.Minute).Unix()})
	mac := hmac.New(sha256.New, []byte("pgsprite-local-test-jwt-secret-not-for-production"))
	_, err := mac.Write([]byte(body))
	require.NoError(t, err)
	return body + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func tenantID(tenant int) string { return fmt.Sprintf("00000000-0000-0000-0000-%012d", tenant) }

func get(ctx context.Context, url, token string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := http.Client{Timeout: 2 * time.Second}
	response, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	closeErr := response.Body.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP status %d", response.StatusCode)
	}
	return body, nil
}

func compose(ctx context.Context, args ...string) error {
	file, err := filepath.Abs("../../compose/supabase.yml")
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "docker", append([]string{"compose", "-p", "pgsprite-supabase", "-f", file}, args...)...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("compose: %w: %s", err, output)
	}
	return nil
}

func TestAPIAndPooler(t *testing.T) {
	pool := fixture(t)
	table := newTable(t, pool, "pgsprite_service_probe")
	execSQL(t, pool, "INSERT INTO "+table+" VALUES (1,'00000000-0000-0000-0000-000000000001','first'),(2,'00000000-0000-0000-0000-000000000002','second')")
	execSQL(t, pool, "NOTIFY pgrst, 'reload schema'")
	verify := func(extra string) {
		t.Helper()
		columns := "id"
		if extra != "" {
			columns += "," + extra
		}
		tokens := []string{token(t, 1), token(t, 2), token(t, 3)}
		require.EventuallyWithT(t, func(c *assert.CollectT) {
			for tenant := 1; tenant <= 3; tenant++ {
				body, err := get(t.Context(), "http://127.0.0.1:55439/pgsprite_service_probe?select="+columns, tokens[tenant-1])
				if !assert.NoError(c, err) {
					continue
				}
				var rows []map[string]any
				if !assert.NoError(c, json.Unmarshal(body, &rows)) {
					continue
				}
				expected := []map[string]any{}
				if tenant < 3 {
					row := map[string]any{"id": float64(tenant)}
					if extra != "" {
						row[extra] = nil
					}
					expected = append(expected, row)
				}
				assert.True(c, reflect.DeepEqual(expected, rows), "unexpected rows for tenant %d", tenant)
			}
		}, 30*time.Second, 200*time.Millisecond)
		var count int
		require.NoError(t, pool.QueryRow(t.Context(), "SELECT count(*) FROM "+table).Scan(&count))
		assert.Equal(t, 2, count)
	}
	verify("")
	change(t, pool, "ALTER TABLE "+table+" ADD COLUMN direct_note text")
	verify("direct_note")
	session := connect(t, sessionURL)
	change(t, session, "ALTER TABLE "+table+" ADD COLUMN session_note text")
	verify("session_note")
	v := change(t, session, "CREATE INDEX pgsprite_service_probe_body ON "+table+"(body)")
	require.Len(t, v.ExecutedSQL, 1)
	assert.Regexp(t, `^CREATE INDEX CONCURRENTLY `, v.ExecutedSQL[0])
	verify("session_note")
	// Remove the independent prepared-statement limitation and assert the real
	// session-affinity guard, rather than accepting any connection failure.
	transaction, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: transactionURL, QueryExecMode: pgx.QueryExecModeExec})
	if transaction != nil {
		t.Cleanup(transaction.Close)
	}
	require.ErrorIs(t, err, dbconn.ErrNoSessionAffinity)
	verify("session_note")
}
