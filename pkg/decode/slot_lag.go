package decode

import (
	"errors"
	"fmt"
	"math"
)

// DefaultSlotLagCeiling is the most WAL a slot may retain before the route
// aborts: one GiB (D11). A slot is cluster state that keeps WAL on the
// volume until it is confirmed or dropped, so the bound is hard and the
// abort fails closed.
const DefaultSlotLagCeiling int64 = 1 << 30

// ErrSlotLagCeiling is the sentinel a SlotLagExceededError unwraps to: the
// slot retains more WAL than the ceiling allows (ST-3).
var ErrSlotLagCeiling = errors.New("replication slot retains more WAL than the lag ceiling allows")

// ErrSlotLost is the sentinel a SlotLostError unwraps to: the slot can no
// longer be read from — the server removed WAL it needed, or the slot is
// gone — which is the modeled state ST-4 enters rather than a crash.
var ErrSlotLost = errors.New("replication slot is lost")

// ErrSlotLagUnknown reports a slot whose retained WAL the server could not
// measure while it is not lost: on a server in recovery, where the write
// position cannot be read. An unknown amount is never read as nothing
// retained, so a ceiling cannot be enforced against it and the check fails
// closed (ST-3).
var ErrSlotLagUnknown = errors.New("replication slot's retained WAL cannot be measured")

// SlotLagExceededError is the typed abort for a slot over the ceiling: what
// the slot retained when it was read and the ceiling it crossed, both in
// WAL bytes. It unwraps to ErrSlotLagCeiling.
type SlotLagExceededError struct {
	Slot     string
	Retained int64
	Ceiling  int64
}

func (e *SlotLagExceededError) Error() string {
	return fmt.Sprintf("slot %s retains %s of WAL, over the %s ceiling (%d > %d bytes); "+
		"the usual cause is a long-open transaction on the source, and the run can resume once it ends",
		e.Slot, humanBytes(e.Retained), humanBytes(e.Ceiling), e.Retained, e.Ceiling)
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

// SlotLostError is the typed state for a slot the route can no longer
// decode from. Found is false when no slot of the name exists any more;
// otherwise WALStatus is the server's verdict, "lost" when WAL the slot
// needed has been removed. It unwraps to ErrSlotLost.
type SlotLostError struct {
	Slot      string
	Found     bool
	WALStatus WALStatus
}

func (e *SlotLostError) Error() string {
	if !e.Found {
		return fmt.Sprintf("slot %s no longer exists", e.Slot)
	}
	return fmt.Sprintf("slot %s is lost: the server reports wal_status %q", e.Slot, e.WALStatus)
}

// Unwrap makes every SlotLostError match ErrSlotLost.
func (e *SlotLostError) Unwrap() error { return ErrSlotLost }

// Lost reports whether the server has given up on the slot: WAL it needs
// has been removed, so no stream can ever read it again (ST-4).
func (s SlotStatus) Lost() bool { return s.WALStatus == WALStatusLost }

// WithinLagCeiling judges the slot against a ceiling in WAL bytes. It is
// nil while the slot retains at most ceiling bytes; a lost slot is a
// *SlotLostError before any measure is read, since nothing it retains can
// be streamed; a slot whose retention the server could not measure is
// ErrSlotLagUnknown rather than within the ceiling; and a slot over it is a
// *SlotLagExceededError carrying both figures. A ceiling below one byte is
// an invariant violation: it would abort every slot.
func (s SlotStatus) WithinLagCeiling(ceiling int64) error {
	if ceiling < 1 {
		return fmt.Errorf("%w: ST-3: slot lag ceiling %d is below one byte", ErrInvariantViolation, ceiling)
	}
	// INV: ST-4 — lost is read from the server's verdict, never inferred
	// from a measure.
	if s.Lost() {
		return &SlotLostError{Slot: s.Name, Found: true, WALStatus: s.WALStatus}
	}
	// INV: ST-3 — an unknown amount is never read as nothing retained.
	if !s.Retained.Known {
		return fmt.Errorf("%w: slot %s", ErrSlotLagUnknown, s.Name)
	}
	if s.Retained.Bytes > ceiling {
		return &SlotLagExceededError{Slot: s.Name, Retained: s.Retained.Bytes, Ceiling: ceiling}
	}
	return nil
}
