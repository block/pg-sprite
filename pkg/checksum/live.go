package checksum

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/decode"
)

// ConfirmedSource answers how far the catch-up has applied the stream: the
// position it last confirmed to the server, below which every change to a
// landed key is committed in the shadow. The applier's Catchup is one.
type ConfirmedSource interface {
	Confirmed() decode.LSN
}

// ErrVerificationUnresolved is the sentinel an UnresolvedError unwraps to:
// a live pass could not decide whether a differing chunk is a divergence
// or a chunk the stream has not yet settled.
var ErrVerificationUnresolved = errors.New("live verification could not settle a chunk")

// UnresolvedCause says why a live pass gave up on a chunk.
type UnresolvedCause string

const (
	// CauseLagging is a catch-up that did not confirm past the position the
	// chunk's digests were read at within LiveOptions.SettleWait.
	CauseLagging UnresolvedCause = "lagging"
	// CauseHot is a chunk whose source or shadow changed on every one of
	// LiveOptions.Attempts rereads, so no two reads could be compared.
	CauseHot UnresolvedCause = "hot"
)

// UnresolvedError is a chunk a live pass could neither read clean nor
// confirm divergent. It is not a finding about the data: the shadow is as
// the pass found it, and the caller decides whether to run the pass again
// or wait for the source to quiet down. It unwraps to
// ErrVerificationUnresolved.
type UnresolvedError struct {
	// Chunk is the closed key range the pass could not settle.
	Chunk copier.Chunk
	// Cause says whether the catch-up fell behind or the chunk kept moving.
	Cause UnresolvedCause
	// Rereads is the number of fresh-snapshot rereads the pass made of the
	// chunk before it gave up.
	Rereads int
	// Source and Shadow are the two digests of the pass's last read.
	Source Digest
	Shadow Digest
	// Captured is the server's insert position when the last read was taken;
	// the pass was waiting for the catch-up to confirm past it.
	Captured decode.LSN
	// Confirmed is the catch-up's confirmed position when the pass gave up.
	Confirmed decode.LSN
}

func (e *UnresolvedError) Error() string {
	switch e.Cause {
	case CauseLagging:
		return fmt.Sprintf("chunk [%d, %d] read at %s is unsettled: the catch-up has confirmed only %s after %d rereads",
			e.Chunk.Lower(), e.Chunk.Upper(), e.Captured, e.Confirmed, e.Rereads)
	case CauseHot:
		return fmt.Sprintf("chunk [%d, %d] changed on every one of %d rereads: source %d rows, shadow %d rows at %s",
			e.Chunk.Lower(), e.Chunk.Upper(), e.Rereads, e.Source.Rows, e.Shadow.Rows, e.Captured)
	default:
		return fmt.Sprintf("chunk [%d, %d] is unsettled after %d rereads", e.Chunk.Lower(), e.Chunk.Upper(), e.Rereads)
	}
}

// Unwrap makes every UnresolvedError match ErrVerificationUnresolved.
func (e *UnresolvedError) Unwrap() error { return ErrVerificationUnresolved }

// Default bounds of a live pass's settling.
const (
	// DefaultSettleAttempts is how many fresh rereads a live pass gives a
	// chunk whose rows keep changing before it reports the chunk unsettled.
	DefaultSettleAttempts = 3
	// DefaultSettleWait is the longest a live pass waits for the catch-up
	// to confirm past the position a chunk was read at.
	DefaultSettleWait = 30 * time.Second
	// DefaultSettlePoll is how often a live pass asks the catch-up for its
	// confirmed position while it waits.
	DefaultSettlePoll = 100 * time.Millisecond
)

// LiveOptions bounds how a live pass settles a chunk that differed while
// the source is written and the catch-up applies the stream. Zero values
// take the defaults; Confirmed has none and is required.
type LiveOptions struct {
	// Confirmed is the catch-up applying the stream to the shadow.
	Confirmed ConfirmedSource
	// Attempts is how many fresh rereads a chunk that keeps changing gets
	// before the pass reports it unsettled.
	Attempts int
	// SettleWait bounds each wait for the catch-up to confirm past the
	// position a chunk was read at.
	SettleWait time.Duration
	// Poll is the interval between reads of the catch-up's confirmed
	// position during a wait.
	Poll time.Duration
	// Sleep waits between polls; nil waits on the wall clock. Tests inject
	// one to drive the settling without real time passing.
	Sleep func(ctx context.Context, d time.Duration) error
}

func (o LiveOptions) withDefaults() LiveOptions {
	if o.Attempts == 0 {
		o.Attempts = DefaultSettleAttempts
	}
	if o.SettleWait == 0 {
		o.SettleWait = DefaultSettleWait
	}
	if o.Poll == 0 {
		o.Poll = DefaultSettlePoll
	}
	if o.Sleep == nil {
		o.Sleep = sleepContext
	}
	return o
}

// validate runs after withDefaults, so every field is set.
func (o LiveOptions) validate() error {
	if o.Confirmed == nil {
		return fmt.Errorf("%w: a live pass needs the catch-up's confirmed position", ErrInvalidOptions)
	}
	if o.Attempts < 1 {
		return fmt.Errorf("%w: settle attempts %d is below one; use zero for the default", ErrInvalidOptions, o.Attempts)
	}
	for _, wait := range []struct {
		name  string
		value time.Duration
	}{{"settle wait", o.SettleWait}, {"settle poll", o.Poll}} {
		if wait.value < time.Millisecond {
			return fmt.Errorf("%w: %s %s is below a millisecond; use zero for the default", ErrInvalidOptions, wait.name, wait.value)
		}
	}
	return nil
}

// sleepContext waits for d or until ctx ends, whichever comes first.
func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// CheckLive runs one verification pass through the copier's landed
// watermark while the source is written and the catch-up applies the
// stream to the shadow, and acts on what it finds as policy says. The
// sweep is the same as Check's: every chunk cut and digested on both sides
// in one snapshot. A chunk that differs in the sweep is not yet a finding,
// since the shadow may simply not have received the source's latest
// writes: the pass records the server's insert position the chunk was read
// at, waits for the catch-up to confirm past it, reads the chunk again in a
// fresh snapshot, and judges the two reads together (settle). A chunk whose
// two sides then agree, or whose shadow has reached exactly what the sweep
// saw on the source, is settled and counts as clean; a chunk that kept
// changing is reread up to LiveOptions.Attempts times and then reported as
// an *UnresolvedError; a chunk that held still on both sides across a
// confirmed position and still differs is a divergence. The sweep's
// premise is that the stream is the shadow's only writer, so the shadow
// either lags the source or has caught up to it, and nothing else moves
// it.
//
// A clean pass — every chunk equal or settled — mints the proofs as Check
// does. Under DivergenceAbort a divergence is returned as a
// *DivergenceError with the shadow untouched. Under DivergenceRepair every
// divergent chunk is recopied from the source in one transaction that
// fences the stream's flushes out of the shadow while it runs, the pass
// waits for the catch-up to confirm past the recopy, and the chunk is
// settled again; a chunk divergent after its repair is a *RepairError. A
// pass with repairs mints no proof (CO-2). DivergenceRecopy is refused: it
// is the reconcile mode's policy for a pass that reads nothing again, and
// a live pass settles every chunk it recopies.
//
// While it runs the verifier is the tracker's work source: the sweep's
// differing chunks count as chunks_mismatched, and every settling reread
// as chunks_reread. A CheckLive that overlaps a pass still running on this
// verifier is refused with ErrPassRunning.
func (v *Verifier) CheckLive(ctx context.Context, pool *pgxpool.Pool, through copier.Watermark, policy DivergencePolicy, live LiveOptions) (Outcome, error) {
	if err := policy.validate(); err != nil {
		return Outcome{}, v.verifyError(err)
	}
	// INV: CO-3
	if policy == DivergenceRecopy {
		return Outcome{}, v.verifyError(fmt.Errorf("%w: %q reads nothing again; a live pass settles every chunk it recopies, so it takes %q or %q", ErrNoDivergencePolicy, DivergenceRecopy, DivergenceAbort, DivergenceRepair))
	}
	live = live.withDefaults()
	if err := live.validate(); err != nil {
		return Outcome{}, v.verifyError(err)
	}
	stop, err := v.report()
	if err != nil {
		return Outcome{}, v.verifyError(err)
	}
	defer stop()
	if !through.Valid() {
		return Outcome{}, fmt.Errorf("%w: %s.%s", ErrNothingLanded, v.target.Schema(), v.target.Table())
	}
	ctx, unbind := v.lock.Bind(ctx)
	defer unbind()
	report, err := v.sweepAndSettle(ctx, pool, through, live)
	if err != nil {
		return Outcome{}, v.lostOr(err)
	}
	if report.Clean() {
		return v.prove(report), nil
	}
	// INV: CO-3
	switch policy {
	case DivergenceAbort:
		return Outcome{Report: report}, v.verifyError(&DivergenceError{Report: report})
	case DivergenceRepair:
		repairs, err := v.repairLive(ctx, pool, report.Mismatches, live)
		if err != nil {
			return Outcome{Report: report, Repairs: repairs}, v.verifyError(v.lostOr(err))
		}
		return Outcome{Report: report, Repairs: repairs}, nil
	default:
		return Outcome{Report: report}, fmt.Errorf("%w (CO-3): divergence policy %q passed validation", ErrInvariantViolation, string(policy))
	}
}

// sweepAndSettle runs the live sweep and settles every chunk it found
// differing, in key order, returning the report with the confirmed
// divergences as its mismatches and the settled chunks counted.
func (v *Verifier) sweepAndSettle(ctx context.Context, pool *pgxpool.Pool, through copier.Watermark, live LiveOptions) (Report, error) {
	sourceSQL, shadowSQL, err := v.digestStatements(ctx, pool)
	if err != nil {
		return Report{}, err
	}
	report, differing, err := v.sweep(ctx, pool, sourceSQL, shadowSQL, through, true)
	if err != nil {
		return Report{}, err
	}
	for _, prior := range differing {
		verdict, now, err := v.settle(ctx, pool, sourceSQL, shadowSQL, prior, live)
		if err != nil {
			return Report{}, err
		}
		switch verdict {
		case verdictEqual, verdictSettled:
			report.Settled++
		case verdictDiverged:
			report.Mismatches = append(report.Mismatches, Mismatch{Chunk: now.chunk, Source: now.source, Shadow: now.shadow})
		default:
			return Report{}, fmt.Errorf("%w (CO-1): settle returned verdict %d", ErrInvariantViolation, verdict)
		}
	}
	if lost := v.lockLost(); lost != nil {
		return Report{}, lost
	}
	return report, nil
}
