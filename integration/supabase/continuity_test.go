package supabase_test

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func verifyAPITenants(t *testing.T, name string, expected map[int][]int) {
	t.Helper()
	for tenant := 1; tenant <= 3; tenant++ {
		jwt := token(t, tenant)
		var body []byte
		// Wait for schema-cache readiness only; never retry a successful response
		// that exposes the wrong tenant's rows.
		require.EventuallyWithT(t, func(c *assert.CollectT) {
			var err error
			body, err = get(t.Context(), "http://127.0.0.1:55439/"+name+"?select=id&order=id", jwt)
			assert.NoError(c, err)
		}, 30*time.Second, 100*time.Millisecond)
		var rows []struct {
			ID int `json:"id"`
		}
		require.NoError(t, json.Unmarshal(body, &rows))
		ids := []int{}
		for _, row := range rows {
			ids = append(ids, row.ID)
		}
		assert.Equal(t, expected[tenant], ids, "API tenant %d", tenant)
	}
}

func runServiceRefusal(t *testing.T, tc rewriteCase) {
	t.Helper()
	pool := fixture(t)
	name := "pgsprite_realtime_probe"
	table := seedDDLTable(t, pool, name)
	startRealtime(t)
	first, second := subscribe(t, 1), subscribe(t, 2)
	subscribers := []*subscription{first, second}
	expected := map[int][]int{1: {1}, 2: {2}, 3: {}}
	verifyAPITenants(t, name, expected)
	execSQL(t, pool, "UPDATE "+table+" SET body=body")
	for _, s := range subscribers {
		key := fmt.Sprintf("UPDATE:%d", s.tenant)
		s.receiveUntil(t, func(_ realtimeMessage) bool { _, ok := s.records[key]; return ok })
	}
	refuseRewrite(t, pool, table, name, tc)
	for _, s := range subscribers {
		id := 10 + (s.tenant-1)*1000
		_, err := pool.Exec(t.Context(), "INSERT INTO "+table+" VALUES ($1,$2,'123','one',12.50,'ready')", id, tenantID(s.tenant))
		require.NoError(t, err)
		key := fmt.Sprintf("INSERT:%d", id)
		s.receiveUntil(t, func(_ realtimeMessage) bool { _, ok := s.records[key]; return ok })
		assert.Equal(t, "123", s.records[key].Payload.Data.Record.Body)
		expected[s.tenant] = append(expected[s.tenant], id)
	}
	verifyAPITenants(t, name, expected)
}
