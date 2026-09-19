package dbconn

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The rendered name is for messages only; the lock key's input is the
// server-side quote_ident form, which keeps "odd.schema"."table" and
// "odd"."schema.table" distinct.
func TestTableLockQualifiedName(t *testing.T) {
	assert.Equal(t, "odd.schema.table.with.dots", tableLockQualifiedName("odd.schema", "table.with.dots"))
}

// Every guard refuses before a connection is dialed, so an empty Config is
// enough to reach it.
func TestAcquireTableLockRefusesInvalidInput(t *testing.T) {
	tests := map[string]struct {
		schema, table string
		options       []TableLockOption
	}{
		"empty schema":           {schema: "", table: "orders"},
		"empty table":            {schema: "app", table: ""},
		"zero keepalive":         {schema: "app", table: "orders", options: []TableLockOption{WithTableLockKeepalive(0)}},
		"negative keepalive":     {schema: "app", table: "orders", options: []TableLockOption{WithTableLockKeepalive(-time.Second)}},
		"nil option":             {schema: "app", table: "orders", options: []TableLockOption{nil}},
		"nil option after valid": {schema: "app", table: "orders", options: []TableLockOption{WithTableLockKeepalive(time.Second), nil}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			session, err := AcquireTableLock(t.Context(), Config{}, tc.schema, tc.table, tc.options...)
			require.ErrorIs(t, err, ErrInvariantViolation)
			assert.Nil(t, session)
		})
	}
}

func TestLookupTableLockHolderRefusesEmptyNames(t *testing.T) {
	_, found, err := LookupTableLockHolder(t.Context(), nil, "", "orders")
	require.ErrorIs(t, err, ErrInvariantViolation)
	assert.False(t, found)
}

// The contention message carries what the catalog knew about the holder and
// no more: a hidden application_name leaves only the backend PID.
func TestTableLockHeldErrorNamesTheHolder(t *testing.T) {
	since := time.Date(2026, time.March, 4, 5, 6, 7, 0, time.UTC)
	tests := map[string]struct {
		err  TableLockHeldError
		want string
	}{
		"holder unknown": {
			err:  TableLockHeldError{Schema: "app", Table: "orders"},
			want: "table lock is already held for app.orders",
		},
		"pid only": {
			err:  TableLockHeldError{Schema: "app", Table: "orders", Holder: TableLockHolder{PID: 66}},
			want: "table lock is already held for app.orders by backend 66",
		},
		"full holder": {
			err: TableLockHeldError{Schema: "app", Table: "orders", Holder: TableLockHolder{
				PID: 66, ApplicationName: "pg-sprite", BackendStart: since, State: "idle"}},
			want: `table lock is already held for app.orders by backend 66 (application_name "pg-sprite", connected since 2026-03-04T05:06:07Z)`,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.err.Error())
		})
	}
}
