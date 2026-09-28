package hosted_test

import (
	"context"
	"fmt"
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
	s.startHeartbeat(t)
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

// Keep long fixture initialization alive using the protocol heartbeat, not reconnects.
// The writer is stopped and joined before the earlier socket-close cleanup runs.
func (s *stream) startHeartbeat(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for ref := 2; ; ref++ {
			select {
			case <-ctx.Done():
				done <- nil
				return
			case <-ticker.C:
				if err := s.conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
					done <- err
					return
				}
				if err := s.conn.WriteJSON(map[string]any{"topic": "phoenix", "event": "heartbeat", "payload": map[string]any{}, "ref": fmt.Sprint(ref)}); err != nil {
					done <- err
					return
				}
			}
		}
	}()
	t.Cleanup(func() { cancel(); assert.NoError(t, <-done, "Realtime heartbeat failed") })
}
