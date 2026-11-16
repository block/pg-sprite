package change

// TableInfo describes a table a subscription tracks. It carries the fields
// of Spirit's table.TableInfo
// (https://github.com/block/spirit/blob/fc9cb5f4feed5b172da761865ff2809e4da9d912/pkg/table/tableinfo.go)
// that a change source reads.
type TableInfo struct {
	// SchemaName is the table's schema.
	SchemaName string
	// TableName is the table's name.
	TableName string
	// Columns is every column, in the order a row image carries them.
	Columns []string
	// KeyColumns is the primary key's columns, in key order.
	KeyColumns []string
}

// MappedChunker is the part of Spirit's table.MappedChunker
// (https://github.com/block/spirit/blob/fc9cb5f4feed5b172da761865ff2809e4da9d912/pkg/table/chunker.go)
// a subscription reads while the copy runs: where a key stands against the
// copy's watermarks. The source never calls it; it hands it to the
// subscription it builds.
type MappedChunker interface {
	// KeyAboveHighWatermark reports whether the copy has not reached key0
	// yet, so a change to it may be discarded: the copy reads it later.
	KeyAboveHighWatermark(key0 any) bool
	// KeyBelowLowWatermark reports whether the copy has passed key0 and
	// committed it.
	KeyBelowLowWatermark(key0 any) bool
	// KeyNotYetDispatched reports whether no chunk covering key0 has been
	// handed out, so no copy read for it is in flight.
	KeyNotYetDispatched(key0 any) bool
}

// TableLock is a lock a caller holds on one target server's tables. The
// source never reads it: it hands every lock to every subscription's Flush,
// and the subscription's applier executes each target's statements under
// that target's lock, so its shape belongs to the applier.
type TableLock any
