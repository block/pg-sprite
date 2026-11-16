package change

import "time"

// FeedStats is what a source reports for a status display. It carries the
// fields of Spirit's change.FeedStats
// (https://github.com/block/spirit/blob/fc9cb5f4feed5b172da761865ff2809e4da9d912/pkg/change/stats.go).
type FeedStats struct {
	// LastFlushAt is when the most recently completed flush finished; zero
	// before the first.
	LastFlushAt time.Time
	// LastFlushDuration is how long that flush took.
	LastFlushDuration time.Duration
	// LastFlushRows is how many changes were pending when that flush
	// started.
	LastFlushRows int
	// BufferedPosition is how far the feed has read, in Position's
	// encoding; empty before it has read anything. It is not the resume
	// position, which advances only when a flush lands every buffered
	// change.
	BufferedPosition string
	// BufferedEventAt is the source's commit time of the newest change the
	// feed has read; zero when the source does not report one.
	BufferedEventAt time.Time
	// Rotations counts log-file rotations the feed followed; zero for a
	// source with no log files.
	Rotations int64
	// ForcedRotations counts rotations the feed forced itself.
	ForcedRotations int64
	// Parks counts how many times a subscription held the reader on its
	// soft limits.
	Parks int64
	// IsParked reports whether a subscription is holding the reader now.
	IsParked bool
	// FlushShape is the width a drain runs at now, and
	// ConfiguredFlushShape the width it would run at with no back-off.
	FlushShape           FlushShape
	ConfiguredFlushShape FlushShape
}

// FlushShape is the width of a drain: how many applier batches run
// concurrently and how many rows each renders into one statement.
type FlushShape struct {
	Concurrency int
	BatchSize   int
}
