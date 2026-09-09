package dbconn

// TableLock proves this process holds LK-1's per-table advisory lock on a
// dedicated session. Its zero value is forgeable; consumers must reject it
// when Table is empty.
type TableLock struct {
	schema, table string
	key           int64
}

// Schema returns the locked table's schema.
func (l TableLock) Schema() string { return l.schema }

// Table returns the locked table name.
func (l TableLock) Table() string { return l.table }

// Key returns the advisory lock key.
func (l TableLock) Key() int64 { return l.key }
