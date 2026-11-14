package migrate

import (
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// This file is the irreversible-operations registry: every operation
// pg-sprite refuses to run, because it cannot be undone, until a typed
// acknowledgement lifts the refusal on the imperative front door, with what
// it loses and that acknowledgement. A loss that is the change the operator
// asked for is not listed: a copy-and-swap without KeepOld drops the
// retained source once the swap has committed, which ends a rollback window
// but discards nothing the new table does not carry. Each
// acknowledgement names the resolved SCHEMA.TABLE, so one left on a script
// cannot reach another table. Desired-state execution takes none of them:
// it refuses a plan holding a destructive statement whole, and rejects
// every acknowledgement before any database work, and
// Options.WithoutAcknowledgements clears the same set. The destructive operation
// shapes are not listed here but derived from the planner, the one answer
// both destructive gates use. TestIrreversibleOperationRegistryIsComplete
// fails when an acknowledgement, a string or bool field on Options or
// CopySwapOptions that could carry one, or a destructive shape arrives
// without an entry, and when a listed entry names the wrong acknowledgement
// or refusal. Acknowledgements exports the set by name.

// ackKind is one typed acknowledgement the imperative front door takes.
// Its value is the migrate command's flag name.
type ackKind string

const (
	ackForce            ackKind = "force"
	ackAcceptBlocking   ackKind = "accept-blocking"
	ackAllowDestructive ackKind = "allow-destructive"
	ackRestart          ackKind = "restart"
	ackBlockWrites      ackKind = "block-writes"
)

// ackKinds is the closed set of acknowledgements, in the order RunDesired
// rejects them.
func ackKinds() []ackKind {
	return []ackKind{ackForce, ackAcceptBlocking, ackAllowDestructive, ackRestart, ackBlockWrites}
}

// field is the Options field that carries the acknowledgement: the one
// place a kind is tied to a field, so reading and clearing it cannot
// disagree. It is nil only for a kind this switch does not name, which the
// registry's completeness test rejects.
func (a ackKind) field(opts *Options) *string {
	switch a {
	case ackForce:
		return &opts.Force
	case ackAcceptBlocking:
		return &opts.AcceptBlocking
	case ackAllowDestructive:
		return &opts.AllowDestructive
	case ackRestart:
		return &opts.CopySwap.Restart
	case ackBlockWrites:
		return &opts.CopySwap.BlockWrites
	default:
		return nil
	}
}

// value is the acknowledgement as opts carries it; empty means not given.
// The bool is false only for a kind field does not name.
func (a ackKind) value(opts Options) (string, bool) {
	f := a.field(&opts)
	if f == nil {
		return "", false
	}
	return *f, true
}

// Acknowledgements returns the name of every typed acknowledgement the
// engine takes, in the order [RunDesired] rejects them: "force",
// "accept-blocking", "allow-destructive", "restart", "block-writes", and
// any added later. Each name is also the migrate command's flag for it.
// An orchestrator that maps its own approval switches onto the engine's
// acknowledgements can assert every one is reachable. Each call returns a
// fresh slice, so changing it changes nothing else.
func Acknowledgements() []string {
	kinds := ackKinds()
	names := make([]string, 0, len(kinds))
	for _, a := range kinds {
		names = append(names, string(a))
	}
	return names
}

// Acknowledgements returns the name, as [Acknowledgements] spells it, of
// every typed acknowledgement o carries, in the same order, or nil when o
// carries none. An adapter calls it before [Options.WithoutAcknowledgements]
// to learn which of an operator's acknowledgements the desired-state view
// sets aside. Each call returns a fresh slice.
func (o Options) Acknowledgements() []string {
	var names []string
	for _, a := range ackKinds() {
		if v, _ := a.value(o); v != "" {
			names = append(names, string(a))
		}
	}
	return names
}

// WithoutAcknowledgements returns a copy of o with every typed
// acknowledgement cleared — Force, AcceptBlocking, AllowDestructive,
// CopySwap.Restart, CopySwap.BlockWrites, and any added later — and every
// other field unchanged. It is the desired-state front door's view of a
// policy: an adapter that builds one Options for both front doors passes
// [RunDesired] o.WithoutAcknowledgements(), which RunDesired never rejects
// for an acknowledgement. o itself is not modified. The view drops an
// operator's consent without reporting it: an adapter that must not lose
// one calls [Options.Acknowledgements] on o first, which names every
// acknowledgement the view sets aside, and the desired-state door's own
// refusals still apply.
func (o Options) WithoutAcknowledgements() Options {
	for _, a := range ackKinds() {
		if f := a.field(&o); f != nil {
			*f = ""
		}
	}
	return o
}

// desiredErr is the sentinel RunDesired returns when the acknowledgement
// is given.
func (a ackKind) desiredErr() error {
	switch a {
	case ackForce:
		return ErrForceNotSupported
	case ackAcceptBlocking:
		return ErrAcceptBlockingNotSupported
	case ackAllowDestructive:
		return ErrAllowDestructiveNotSupported
	case ackRestart:
		return ErrRestartNotSupported
	case ackBlockWrites:
		return ErrBlockWritesNotSupported
	default:
		return nil
	}
}

// rejectDesiredAcks returns the sentinel for the first acknowledgement opts
// carries, or nil when it carries none.
func rejectDesiredAcks(opts Options) error {
	for _, a := range ackKinds() {
		if v, _ := a.value(opts); v != "" {
			return a.desiredErr()
		}
	}
	return nil
}

// loss is what an irreversible operation cannot give back.
type loss string

const (
	// lossStructure: the planner marks the shape destructive — a dropped
	// column's values, a constraint's or NOT NULL's guarantee, an index.
	lossStructure loss = "data-or-structure"
	// lossAvailability: the table's reads or writes stop for the length of
	// the operation.
	lossAvailability loss = "availability"
	// lossProgress: a stopped copy-and-swap's shadow, checkpoint row, and
	// write fence or replication slot.
	lossProgress loss = "progress"
)

// desiredHandling is what desired-state execution does about an entry.
type desiredHandling string

const (
	// desiredRefusesPlan: the planner can derive the operation, and
	// admission refuses any plan holding it, whole.
	desiredRefusesPlan desiredHandling = "refuses-plan"
	// desiredRejectsAck: no plan derives the operation on its own, and the
	// acknowledgement that would allow it is rejected.
	desiredRejectsAck desiredHandling = "rejects-ack"
)

// irreversibleOp is one registry entry.
type irreversibleOp struct {
	// key names the operation: a destructive shape, a blocking statement
	// kind, the submitted form, or a copy-and-swap lifecycle action.
	key string
	// loss is what the operation cannot give back.
	loss loss
	// ack lifts the refusal on the imperative front door.
	ack ackKind
	// refusal is the reason the operation is refused with when ack is
	// missing. Empty only for a progress loss: without the acknowledgement
	// the run resumes, so nothing is discarded and nothing is refused.
	refusal verdict.Reason
	// desired is what desired-state execution does about it.
	desired desiredHandling
}

// irreversibleOps is the registry: one entry per destructive operation
// shape, derived from the planner, followed by the operations no shape
// classifies.
func irreversibleOps() []irreversibleOp {
	// INV: RF-12 — every operation a typed acknowledgement lifts is listed
	// with that acknowledgement.
	var ops []irreversibleOp
	for _, k := range planner.DestructiveOpKinds() {
		ops = append(ops, irreversibleOp{
			key:     "shape: " + statement.Op{Kind: k}.Describe(),
			loss:    lossStructure,
			ack:     ackAllowDestructive,
			refusal: verdict.ReasonDestructiveChange,
			desired: desiredRefusesPlan,
		})
	}
	return append(ops,
		irreversibleOp{
			key:     "blocking DROP INDEX",
			loss:    lossAvailability,
			ack:     ackAcceptBlocking,
			refusal: verdict.ReasonIndexStatement,
			desired: desiredRejectsAck,
		},
		irreversibleOp{
			key:     "blocking REINDEX",
			loss:    lossAvailability,
			ack:     ackAcceptBlocking,
			refusal: verdict.ReasonIndexStatement,
			desired: desiredRejectsAck,
		},
		irreversibleOp{
			key:     "submitted form that blocks the table",
			loss:    lossAvailability,
			ack:     ackForce,
			refusal: verdict.ReasonRewriteRequired,
			desired: desiredRejectsAck,
		},
		irreversibleOp{
			key:     "copy-and-swap restart",
			loss:    lossProgress,
			ack:     ackRestart,
			desired: desiredRejectsAck,
		},
		irreversibleOp{
			key:     "copy-and-swap with the table's writes blocked",
			loss:    lossAvailability,
			ack:     ackBlockWrites,
			refusal: verdict.ReasonCopyAndSwapRefused,
			desired: desiredRejectsAck,
		},
	)
}
