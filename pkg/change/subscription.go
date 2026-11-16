package change

import "context"

// Subscription buffers the changes to one table and applies them to the
// target. It declares the same methods as Spirit's change.Subscription
// (https://github.com/block/spirit/blob/fc9cb5f4feed5b172da761865ff2809e4da9d912/pkg/change/subscription.go).
type Subscription interface {
	// HasChanged records a change: key is the primary-key values in the
	// table's key order, row the whole image in the table's column order,
	// and deleted marks a deletion, for which row is not read.
	HasChanged(key, row []any, deleted bool)
	// Length is the number of changes buffered.
	Length() int
	// Flush applies the buffered changes. When underLock is true, locks
	// carries the table locks the caller holds, one per target server.
	// allChangesFlushed is false when changes were left buffered.
	Flush(ctx context.Context, underLock bool, locks []TableLock) (allChangesFlushed bool, err error)
	// Tables is the subscription's tables in currentTable, newTable order;
	// only [currentTable] when it has no destination-side table. Entries
	// are never nil.
	Tables() []*TableInfo
	// ImmutableColumnOrdinal is the index into Tables()[0].Columns of a
	// column whose value must never change in an UPDATE, or -1 when there
	// is none. A source treats an UPDATE that changes it as fatal.
	ImmutableColumnOrdinal() int
	// SetWatermarkOptimization turns the watermark optimization on or off,
	// draining the buffer through the applier when the mode changes.
	SetWatermarkOptimization(ctx context.Context, enabled bool) error
	// Close signals that no further changes will be delivered and releases
	// a HasChanged caller held on backpressure. It does not flush. Safe to
	// call more than once.
	Close()
}
