package hosted_test

import (
	"net/url"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type event struct {
	Event   string `json:"event"`
	Payload struct {
		Status    string `json:"status"`
		Extension string `json:"extension"`
		Message   string `json:"message"`
		Data      struct {
			Type   string `json:"type"`
			Record struct {
				ID    int    `json:"id"`
				Owner string `json:"owner_id"`
			} `json:"record"`
		} `json:"data"`
	} `json:"payload"`
}
type stream struct {
	conn  *websocket.Conn
	owner string
}

func (f *fixture) subscribe(t *testing.T, tenant int) *stream {
	t.Helper()
	endpoint, err := url.Parse(f.base)
	require.NoError(t, err)
	endpoint.Scheme = "wss"
	endpoint.Path = "/realtime/v1/websocket"
	endpoint.RawQuery = url.Values{"apikey": {f.keys.Publishable}, "vsn": {"1.0.0"}}.Encode()
	conn, response, err := websocket.DefaultDialer.DialContext(t.Context(), endpoint.String(), nil)
	if response != nil && response.Body != nil {
		require.NoError(t, response.Body.Close())
	}
	// Do not include the URL in failure diagnostics; it contains the API key.
	require.True(t, err == nil, "Realtime WebSocket connection failed")
	t.Cleanup(func() { assert.NoError(t, conn.Close()) })
	conn.SetReadLimit(1 << 20)
	require.NoError(t, conn.SetWriteDeadline(time.Now().Add(5*time.Second)))
	require.NoError(t, conn.WriteJSON(map[string]any{
		"topic": "realtime:" + f.name, "event": "phx_join", "ref": "1", "join_ref": "1",
		"payload": map[string]any{"access_token": f.users[tenant].Token, "config": map[string]any{"postgres_changes": []map[string]any{{"event": "*", "schema": "public", "table": f.name}}}},
	}))
	s := &stream{conn: conn, owner: f.users[tenant].ID}
	s.await(t, func(e event) bool {
		return e.Event == "system" && e.Payload.Extension == "postgres_changes" && e.Payload.Status == "ok"
	})
	return s
}

func (s *stream) await(t *testing.T, match func(event) bool) {
	t.Helper()
	const eventDeadline = 30 * time.Second
	require.NoError(t, s.conn.SetReadDeadline(time.Now().Add(eventDeadline)))
	for {
		var e event
		err := s.conn.ReadJSON(&e)
		require.NoError(t, err, "waiting for hosted Realtime event")
		if e.Event == "system" {
			t.Logf("Realtime system status=%s message=%s", e.Payload.Status, e.Payload.Message)
		}
		require.NotEqual(t, "phx_error", e.Event)
		require.NotEqual(t, "phx_close", e.Event)
		require.NotEqual(t, "error", e.Payload.Status, "Realtime system error: %s", e.Payload.Message)
		if e.Event == "postgres_changes" {
			require.Equal(t, s.owner, e.Payload.Data.Record.Owner, "cross-tenant event")
		}
		if match(e) {
			return
		}
	}
}
func (s *stream) row(t *testing.T, operation string, id int) {
	t.Helper()
	s.await(t, func(e event) bool {
		return e.Event == "postgres_changes" && e.Payload.Data.Type == operation && e.Payload.Data.Record.ID == id
	})
}

// This probe separates subscription acknowledgements from actual delivery.
// It deliberately does not run pg-sprite DDL, reconnect, or retry missing events.
func TestHostedRealtimeBaseline(t *testing.T) {
	f := newFixture(t)
	f.seed(t)
	first, second := f.subscribe(t, 0), f.subscribe(t, 1)
	for _, u := range f.users {
		var count int
		require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT count(*) FROM realtime.subscription WHERE entity=$1::regclass AND claims->>'sub'=$2 AND claims->>'role'='authenticated'", f.table, u.ID).Scan(&count))
		require.Equal(t, 1, count, "server must register the correct table and identity")
	}
	f.exec(t, "INSERT INTO "+f.table+" VALUES (3,$1,'probe'),(4,$2,'probe')", f.users[0].ID, f.users[1].ID)
	first.row(t, "INSERT", 3)
	second.row(t, "INSERT", 4)
	f.exec(t, "UPDATE "+f.table+" SET body='changed' WHERE id IN (3,4)")
	first.row(t, "UPDATE", 3)
	second.row(t, "UPDATE", 4)
	t.Log("received INSERT and UPDATE for both tenants without schema changes")
}
