package hosted_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type credentials struct {
	Publishable string `json:"publishable_key"`
	Secret      string `json:"secret_key"`
}
type user struct {
	ID    string `json:"id"`
	Token string `json:"access_token"`
}
type fixture struct {
	pool   *pgxpool.Pool
	base   string
	keys   credentials
	users  []user
	client *http.Client
	name   string
	table  string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	if os.Getenv("SUPABASE_HOSTED_TEST") != "1" {
		t.Skip("set SUPABASE_HOSTED_TEST=1 for a disposable hosted project")
	}
	base := os.Getenv("SUPABASE_HOSTED_URL")
	parsed, err := url.Parse(base)
	require.NoError(t, err)
	require.Equal(t, "https", parsed.Scheme)
	require.Nil(t, parsed.User)
	require.Empty(t, parsed.RawQuery)
	require.Empty(t, parsed.Fragment)
	require.Empty(t, parsed.Path)
	ref := strings.TrimSuffix(parsed.Host, ".supabase.co")
	require.NotEmpty(t, ref)
	require.NotContains(t, ref, ".")
	require.Equal(t, ref+".supabase.co", parsed.Host)
	dsn := os.Getenv("PGSPRITE_URL")
	cfg, err := pgx.ParseConfig(dsn)
	require.NoError(t, err)
	require.Equal(t, "db."+ref+".supabase.co", cfg.Host, "use this project's direct database URL for the baseline")
	path := os.Getenv("SUPABASE_HOSTED_CREDENTIALS")
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Zero(t, info.Mode().Perm()&0077, "credentials file must be private")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var keys credentials
	require.NoError(t, json.Unmarshal(data, &keys))
	require.NotEmpty(t, keys.Publishable)
	require.NotEmpty(t, keys.Secret)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: dsn})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	f := &fixture{pool: pool, base: base, keys: keys, client: &http.Client{Timeout: 15 * time.Second}}
	for range 2 {
		f.users = append(f.users, f.createUser(t))
	}
	f.name = "pgsprite_hosted_" + randomID(t)
	f.table = pgx.Identifier{"public", f.name}.Sanitize()
	return f
}

func randomID(t *testing.T) string {
	t.Helper()
	b := make([]byte, 8)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return hex.EncodeToString(b)
}

// Administrative requests never print their response bodies: those can contain tokens.
func (f *fixture) request(ctx context.Context, method, path, key, token string, payload any) (int, []byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, f.base+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("apikey", key)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Prefer", "return=representation")
	response, err := f.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	closeErr := response.Body.Close()
	if readErr != nil {
		return 0, nil, readErr
	}
	if closeErr != nil {
		return 0, nil, closeErr
	}
	return response.StatusCode, data, nil
}

func (f *fixture) createUser(t *testing.T) user {
	t.Helper()
	email := "pgsprite-" + randomID(t) + "@example.com"
	password := randomID(t) + randomID(t)
	status, data, err := f.request(t.Context(), http.MethodPost, "/auth/v1/admin/users", f.keys.Secret, "", map[string]any{"email": email, "password": password, "email_confirm": true})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	var u user
	require.NoError(t, json.Unmarshal(data, &u))
	require.NotEmpty(t, u.ID)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 15*time.Second)
		defer cancel()
		status, _, err := f.request(ctx, http.MethodDelete, "/auth/v1/admin/users/"+url.PathEscape(u.ID), f.keys.Secret, "", nil)
		assert.NoError(t, err)
		assert.Equal(t, http.StatusOK, status)
	})
	status, data, err = f.request(t.Context(), http.MethodPost, "/auth/v1/token?grant_type=password", f.keys.Publishable, "", map[string]string{"email": email, "password": password})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	var session user
	require.NoError(t, json.Unmarshal(data, &session))
	require.NotEmpty(t, session.Token)
	u.Token = session.Token
	return u
}

func (f *fixture) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	_, err := f.pool.Exec(t.Context(), sql, args...)
	require.NoError(t, err)
}
func (f *fixture) seed(t *testing.T) {
	t.Helper()
	f.seedTable(t)
	f.exec(t, "ALTER PUBLICATION supabase_realtime ADD TABLE "+f.table)
	f.waitForAPI(t)
	f.assertAccess(t)
}

func (f *fixture) seedUnpublished(t *testing.T) {
	t.Helper()
	f.seedTable(t)
	f.waitForAPI(t)
	f.assertAccess(t)
}

func (f *fixture) seedTable(t *testing.T) {
	t.Helper()
	f.prepareTableCleanup(t)
	f.exec(t, fmt.Sprintf(`CREATE TABLE %s (
 id integer PRIMARY KEY,
 owner_id uuid NOT NULL,
 body text NOT NULL
 )`, f.table))
	f.exec(t, "ALTER TABLE "+f.table+" ENABLE ROW LEVEL SECURITY")
	f.exec(t, "CREATE POLICY own_rows ON "+f.table+" TO authenticated USING (owner_id = auth.uid()) WITH CHECK (owner_id = auth.uid())")
	f.exec(t, "GRANT SELECT, INSERT, UPDATE, DELETE ON "+f.table+" TO authenticated, anon")
	f.exec(t, "INSERT INTO "+f.table+" VALUES (1,$1,'first'),(2,$2,'second')", f.users[0].ID, f.users[1].ID)
}

func (f *fixture) waitForAPI(t *testing.T) {
	t.Helper()
	f.exec(t, "NOTIFY pgrst, 'reload schema'")
	const cacheDeadline = 30 * time.Second
	require.Eventually(t, func() bool {
		status, _, err := f.request(t.Context(), http.MethodGet, "/rest/v1/"+f.name+"?select=id", f.keys.Publishable, f.users[0].Token, nil)
		return err == nil && status == http.StatusOK
	}, cacheDeadline, 200*time.Millisecond, "Data API must discover the fixture")
}

func (f *fixture) assertRows(t *testing.T, token string, want ...int) {
	t.Helper()
	status, data, err := f.request(t.Context(), http.MethodGet, "/rest/v1/"+f.name+"?select=id&order=id", f.keys.Publishable, token, nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	var rows []struct {
		ID int `json:"id"`
	}
	require.NoError(t, json.Unmarshal(data, &rows))
	ids := make([]int, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	assert.Equal(t, append([]int{}, want...), ids)
}
func (f *fixture) assertAccess(t *testing.T) {
	t.Helper()
	f.assertRows(t, f.users[0].Token, 1)
	f.assertRows(t, f.users[1].Token, 2)
	f.assertRows(t, "")
}
