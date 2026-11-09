package decode

import (
	"errors"
	"fmt"
	"math"
	"time"
)

// DefaultSlotLagCeiling is the most WAL a slot may retain before the route
// aborts: one GiB (D11). A slot is cluster state that keeps WAL on the
// volume until it is confirmed or dropped, so the bound is hard and the
// abort fails closed.
const DefaultSlotLagCeiling int64 = 1 << 30

// RestartReleaseInterval is the longest the server goes between the
// running-transactions records it logs on a primary, busy or quiet (the
// background writer's snapshot interval). A logical slot's restart
// position advances only at those records, once its consumer has
// confirmed past one, so WAL the consumer has confirmed is released in
// steps at most this far apart: a slot that retains more than its
// consumer has yet to confirm has not necessarily been left behind by
// anyone until this long has passed.
const RestartReleaseInterval = 15 * time.Second

// ErrSlotLagCeiling is the sentinel a SlotLagExceededError unwraps to: the
// slot retains more WAL than the ceiling allows (ST-3).
var ErrSlotLagCeiling = errors.New("replication slot retains more WAL than the lag ceiling allows")

// ErrSlotLost is the sentinel a SlotLostError unwraps to: the slot can no
// longer be read from — the server removed WAL it needed, or the slot is
// gone — which is the modeled state ST-4 enters rather than a crash.
var ErrSlotLost = errors.New("replication slot is lost")

// ErrSlotLagUnknown reports a slot whose retained or unconfirmed WAL the
// server could not measure while it is not lost: on a server in recovery,
// where the write position cannot be read. An unknown amount is never
// read as nothing retained, so a ceiling cannot be enforced against it
// and the check fails closed (ST-3).
var ErrSlotLagUnknown = errors.New("replication slot's retained WAL cannot be measured")

// SlotLagCause is which measure of a slot crossed the ceiling, and so
// what an operator looks at.
type SlotLagCause string

const (
	// SlotLagStreamBehind means the WAL the slot's consumer has yet to
	// confirm is over the ceiling: the stream has fallen behind the
	// source's writes, which is the engine's own doing.
	SlotLagStreamBehind SlotLagCause = "stream-behind"
	// SlotLagWALRetained means the WAL the slot keeps on the volume has
	// stayed over the ceiling for longer than the server takes to release
	// what its consumer confirmed, while the unconfirmed WAL itself is
	// within it: the server is keeping WAL back, as for a transaction
	// still open on the source, or the source writes WAL faster than the
	// ceiling allows for the server's release interval.
	SlotLagWALRetained SlotLagCause = "wal-retained"
)

// SlotLagExceededError is the typed abort for a slot over the ceiling:
// which measure crossed it, both measures as the slot was read, and the
// ceiling, all in WAL bytes. It unwraps to ErrSlotLagCeiling.
type SlotLagExceededError struct {
	Slot  string
	Cause SlotLagCause
	// Retained is what the slot kept on the volume; Unconfirmed is the
	// share of it the consumer had yet to confirm.
	Retained    int64
	Unconfirmed int64
	Ceiling     int64
}

func (e *SlotLagExceededError) Error() string {
	switch e.Cause {
	case SlotLagStreamBehind:
		return fmt.Sprintf("slot %s has %s of WAL its stream has not confirmed, over the %s ceiling (%d > %d bytes); "+
			"the stream has fallen behind the source's writes, and running the schema change again resumes it from the slot once the write rate allows",
			e.Slot, humanBytes(e.Unconfirmed), humanBytes(e.Ceiling), e.Unconfirmed, e.Ceiling)
	default:
		return fmt.Sprintf("slot %s retains %s of WAL, over the %s ceiling (%d > %d bytes), for longer than the server takes to release WAL the stream confirmed, "+
			"and the stream itself is %s behind; the usual cause is a long-open transaction on the source, and the run can resume once it ends",
			e.Slot, humanBytes(e.Retained), humanBytes(e.Ceiling), e.Retained, e.Ceiling, humanBytes(e.Unconfirmed))
	}
}

// humanBytes renders a byte count in binary units, the convention
// PostgreSQL's own size settings use: exact when the count is a whole
// number of the unit, else to one decimal.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB", "EiB"}
	value := float64(n) / unit
	i := 0
	for value >= unit && i < len(units)-1 {
		value /= unit
		i++
	}
	if value == math.Trunc(value) {
		return fmt.Sprintf("%.0f %s", value, units[i])
	}
	return fmt.Sprintf("%.1f %s", value, units[i])
}

// Unwrap makes every SlotLagExceededError match ErrSlotLagCeiling.
func (e *SlotLagExceededError) Unwrap() error { return ErrSlotLagCeiling }

// SlotLagWatch judges successive readings of one slot against a ceiling
// in WAL bytes (D11, ST-3). It judges two measures. The unconfirmed WAL
// is the stream's own account — everything its consumer has yet to
// confirm — and one reading over the ceiling is the abort at once. The
// retained WAL is what the slot keeps on the volume, which also holds WAL
// the consumer confirmed that the server has not yet released, so a
// reading over the ceiling stands for a grace — the server's release
// interval, plus however long the consumer takes to confirm past the
// record that lets the release happen — and is the abort only once the
// readings have stayed over it for that long: by then a server that was
// going to release the WAL has had every chance to. One watch serves one
// slot's readings in order; it is not safe for concurrent use.
type SlotLagWatch struct {
	ceiling int64
	grace   time.Duration
	// overSince is when the current run of readings with the retained WAL
	// over the ceiling began; zero while the last reading was within it.
	overSince time.Time
}

// NewSlotLagWatch prepares a watch against ceiling, letting the retained
// WAL stand over it for grace. A ceiling below one byte would abort every
// slot and a negative grace is no grace, so both are refused as invariant
// violations; a zero grace judges the retained WAL as it judges the
// unconfirmed, on one reading.
func NewSlotLagWatch(ceiling int64, grace time.Duration) (*SlotLagWatch, error) {
	if ceiling < 1 {
		return nil, fmt.Errorf("%w: ST-3: slot lag ceiling %d is below one byte", ErrInvariantViolation, ceiling)
	}
	if grace < 0 {
		return nil, fmt.Errorf("%w: ST-3: slot lag grace %s is negative", ErrInvariantViolation, grace)
	}
	return &SlotLagWatch{ceiling: ceiling, grace: grace}, nil
}

// Judge takes one reading of the slot, made at now by the caller's clock.
// It is nil while the slot is within the ceiling; a lost slot is a
// *SlotLostError before any measure is read, since nothing it retains can
// be streamed; a slot whose measures the server could not take is
// ErrSlotLagUnknown rather than within the ceiling; and a slot over it is
// a *SlotLagExceededError naming the measure that crossed.
func (w *SlotLagWatch) Judge(s SlotStatus, now time.Time) error {
	// INV: ST-4 — lost is read from the server's verdict, never inferred
	// from a measure.
	if lost := s.LostState(); lost != nil {
		return lost
	}
	// INV: ST-3 — an unknown amount is never read as nothing retained.
	if !s.Retained.Known || !s.Unconfirmed.Known {
		return fmt.Errorf("%w: slot %s", ErrSlotLagUnknown, s.Name)
	}
	exceeded := &SlotLagExceededError{Slot: s.Name, Retained: s.Retained.Bytes, Unconfirmed: s.Unconfirmed.Bytes, Ceiling: w.ceiling}
	if s.Unconfirmed.Bytes > w.ceiling {
		exceeded.Cause = SlotLagStreamBehind
		return exceeded
	}
	if s.Retained.Bytes <= w.ceiling {
		w.overSince = time.Time{}
		return nil
	}
	if w.overSince.IsZero() {
		w.overSince = now
	}
	if now.Sub(w.overSince) < w.grace {
		return nil
	}
	exceeded.Cause = SlotLagWALRetained
	return exceeded
}

// SlotLostError is the typed state for a slot the route can no longer
// decode from. Found is false when no slot of the name exists any more;
// otherwise WALStatus is the server's verdict, "lost" when WAL the slot
// needed has been removed, and Invalidation is the server's cause for it
// when the server reports one. It unwraps to ErrSlotLost.
type SlotLostError struct {
	Slot         string
	Found        bool
	WALStatus    WALStatus
	Invalidation SlotInvalidation
}

func (e *SlotLostError) Error() string {
	if !e.Found {
		return fmt.Sprintf("slot %s no longer exists", e.Slot)
	}
	return fmt.Sprintf("slot %s is lost: the server reports wal_status %q (%s)", e.Slot, e.WALStatus, e.Invalidation)
}

// Unwrap makes every SlotLostError match ErrSlotLost.
func (e *SlotLostError) Unwrap() error { return ErrSlotLost }

// Lost reports whether the server has given up on the slot: WAL it needs
// has been removed, so no stream can ever read it again (ST-4).
func (s SlotStatus) Lost() bool { return s.WALStatus == WALStatusLost }

// LostState is the typed state for a slot the server reports lost, carrying
// the server's verdict and its cause; it is nil while the slot is readable,
// and a caller returns it as an error only after checking that.
func (s SlotStatus) LostState() *SlotLostError {
	if !s.Lost() {
		return nil
	}
	return &SlotLostError{Slot: s.Name, Found: true, WALStatus: s.WALStatus, Invalidation: s.Invalidation}
}
