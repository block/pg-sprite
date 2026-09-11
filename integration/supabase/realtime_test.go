package supabase_test

import (
	"context"
	"fmt"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type realtimeMessage struct {
	Event   string `json:"event"`
	Payload struct {
		Status    string `json:"status"`
		Extension string `json:"extension"`
		Data      struct {
			Type   string `json:"type"`
			Record struct {
				ID      int     `json:"id"`
				OwnerID string  `json:"owner_id"`
				Body    string  `json:"body"`
				Title   *string `json:"title"`
			} `json:"record"`
		} `json:"data"`
	} `json:"payload"`
}

type subscription struct {
	messages chan realtimeMessage
	errors   chan error
	records  map[string]realtimeMessage
	tenant   int
}

// Use the published Phoenix protocol over a real socket, without a mock server:
// https://supabase.com/docs/guides/realtime/protocol
func subscribe(t *testing.T, tenant int) *subscription {
	t.Helper()
	jwt := token(t, tenant)
	endpoint := "ws://localhost:55442/socket/websocket?apikey=" + url.QueryEscape(jwt) + "&vsn=1.0.0"
	conn, response, err := websocket.DefaultDialer.DialContext(t.Context(), endpoint, nil)
	if response != nil && response.Body != nil {
		require.NoError(t, response.Body.Close())
	}
	require.NoError(t, err)
	conn.SetReadLimit(1 << 20)
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(time.Minute)))
	require.NoError(t, conn.SetWriteDeadline(time.Now().Add(5*time.Second)))
	s := &subscription{messages: make(chan realtimeMessage, 2048), errors: make(chan error, 1), records: make(map[string]realtimeMessage), tenant: tenant}
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(func() { cancel(); assert.NoError(t, conn.Close()); <-done })
	go func() {
		defer close(done)
		for {
			var msg realtimeMessage
			if err := conn.ReadJSON(&msg); err != nil {
				s.errors <- err
				return
			}
			select {
			case s.messages <- msg:
			case <-ctx.Done():
				return
			}
		}
	}()
	require.NoError(t, conn.WriteJSON(map[string]any{
		"topic": fmt.Sprintf("realtime:pgsprite-tenant-%d", tenant), "event": "phx_join", "ref": "1", "join_ref": "1",
		"payload": map[string]any{"access_token": jwt, "config": map[string]any{"postgres_changes": []map[string]any{{"event": "*", "schema": "public", "table": "pgsprite_realtime_probe"}}}},
	}))
	s.receiveUntil(t, func(msg realtimeMessage) bool {
		return msg.Event == "system" && msg.Payload.Extension == "postgres_changes" && msg.Payload.Status == "ok"
	})
	return s
}

func (s *subscription) receiveUntil(t *testing.T, check func(realtimeMessage) bool) {
	t.Helper()
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	for {
		select {
		case err := <-s.errors:
			require.NoError(t, err, "tenant %d socket", s.tenant)
		case <-timer.C:
			t.Fatalf("tenant %d: timed out awaiting Realtime events", s.tenant)
		case <-t.Context().Done():
			t.Fatal(t.Context().Err())
		case msg := <-s.messages:
			require.NotEqual(t, "error", msg.Payload.Status)
			require.NotEqual(t, "phx_error", msg.Event)
			require.NotEqual(t, "phx_close", msg.Event)
			if msg.Event == "postgres_changes" {
				require.Equal(t, tenantID(s.tenant), msg.Payload.Data.Record.OwnerID, "cross-tenant event")
				s.records[fmt.Sprintf("%s:%d", msg.Payload.Data.Type, msg.Payload.Data.Record.ID)] = msg
			}
			if check(msg) {
				return
			}
		}
	}
}

func TestRealtimeDuringNativeChanges(t *testing.T) {
	pool := fixture(t)
	table := newTable(t, pool, "pgsprite_realtime_probe")
	execSQL(t, pool, "INSERT INTO "+table+" SELECT n+100000,'00000000-0000-0000-0000-000000000003',md5(n::text) FROM generate_series(1,100000) n")
	execSQL(t, pool, "ALTER PUBLICATION supabase_realtime ADD TABLE "+table)
	var oid uint32
	require.NoError(t, pool.QueryRow(t.Context(), "SELECT $1::regclass::oid", table).Scan(&oid))
	startRealtime(t)
	first, second := subscribe(t, 1), subscribe(t, 2)
	execSQL(t, pool, "INSERT INTO "+table+" VALUES (0,'00000000-0000-0000-0000-000000000001','before'),(1000,'00000000-0000-0000-0000-000000000002','before')")
	for _, s := range []*subscription{first, second} {
		s.receiveUntil(t, func(_ realtimeMessage) bool { return len(s.records) == 1 })
	}

	var committed atomic.Int32
	writerCtx, stopWriter := context.WithTimeout(t.Context(), 30*time.Second)
	writerDone := make(chan struct{})
	finishWrites := make(chan struct{})
	writerErrors := make(chan error, 1)
	t.Cleanup(func() { stopWriter(); <-writerDone })
	go func() {
		defer close(writerDone)
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for n := 1; ; n++ {
			select {
			case <-finishWrites:
				return
			case <-writerCtx.Done():
				writerErrors <- writerCtx.Err()
				return
			case <-ticker.C:
			}
			_, err := pool.Exec(writerCtx, "INSERT INTO "+table+" (id,owner_id,body) VALUES ($1,$2,'insert'),($3,$4,'insert')", n, tenantID(1), n+1000, tenantID(2))
			if err != nil {
				writerErrors <- err
				return
			}
			_, err = pool.Exec(writerCtx, "UPDATE "+table+" SET body='updated' WHERE id IN ($1,$2)", n, n+1000)
			if err != nil {
				writerErrors <- err
				return
			}
			committed.Store(int32(n))
		}
	}()
	require.Eventually(t, func() bool { return committed.Load() >= 2 }, 5*time.Second, 10*time.Millisecond)
	session := connect(t, sessionURL)
	start := committed.Load()
	change(t, session, "ALTER TABLE "+table+" ADD COLUMN title text")
	v := change(t, session, "CREATE INDEX pgsprite_realtime_probe_body ON "+table+"(body)")
	require.Len(t, v.ExecutedSQL, 1)
	assert.Regexp(t, `^CREATE INDEX CONCURRENTLY `, v.ExecutedSQL[0])
	assert.Greater(t, committed.Load(), start, "writes must commit during schema changes")
	close(finishWrites)
	<-writerDone
	select {
	case err := <-writerErrors:
		require.NoError(t, err)
	default:
	}
	execSQL(t, pool, "INSERT INTO "+table+" VALUES (999,'00000000-0000-0000-0000-000000000001','after','new column'),(1999,'00000000-0000-0000-0000-000000000002','after','new column')")
	written := int(committed.Load())
	for _, s := range []*subscription{first, second} {
		s.receiveUntil(t, func(_ realtimeMessage) bool { return len(s.records) == written*2+2 })
		offset := (s.tenant - 1) * 1000
		for n := 1; n <= written; n++ {
			assert.Equal(t, "insert", s.records[fmt.Sprintf("INSERT:%d", n+offset)].Payload.Data.Record.Body)
			assert.Equal(t, "updated", s.records[fmt.Sprintf("UPDATE:%d", n+offset)].Payload.Data.Record.Body)
		}
		title := s.records[fmt.Sprintf("INSERT:%d", 999+offset)].Payload.Data.Record.Title
		require.NotNil(t, title)
		assert.Equal(t, "new column", *title)
	}
	t.Logf("received all %d expected events across both tenant subscriptions", written*4+4)
	var count int
	require.NoError(t, pool.QueryRow(t.Context(), "SELECT count(*) FROM "+table).Scan(&count))
	assert.Equal(t, 100004+written*2, count)
	var currentOID uint32
	var enabled, valid bool
	require.NoError(t, pool.QueryRow(t.Context(), "SELECT oid,relrowsecurity FROM pg_class WHERE oid=$1::regclass", table).Scan(&currentOID, &enabled))
	assert.Equal(t, oid, currentOID)
	assert.True(t, enabled)
	require.NoError(t, pool.QueryRow(t.Context(), "SELECT count(*) FROM pg_publication_tables WHERE pubname='supabase_realtime' AND tablename='pgsprite_realtime_probe'").Scan(&count))
	assert.Equal(t, 1, count)
	require.NoError(t, pool.QueryRow(t.Context(), "SELECT indisvalid FROM pg_index WHERE indexrelid='public.pgsprite_realtime_probe_body'::regclass").Scan(&valid))
	assert.True(t, valid)
}

func startRealtime(t *testing.T) {
	t.Helper()
	// Fixture recreation changes table identities cached by Realtime. Reset only
	// before subscribing; never restart a service or reconnect during the changes.
	setupCtx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	require.NoError(t, compose(setupCtx, "up", "-d", "--no-deps", "--force-recreate", "realtime"))
	jwt := token(t, 1)
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		// The image's temporary seed process also serves HTTP; wait for the marker
		// created by the final server command before accepting health responses.
		if !assert.NoError(c, compose(setupCtx, "exec", "-T", "realtime", "test", "-f", "/tmp/pgsprite-server-ready")) {
			return
		}
		_, err := get(setupCtx, "http://localhost:55442/api/tenants/localhost/health", jwt)
		assert.NoError(c, err)
	}, 30*time.Second, 200*time.Millisecond)
}
