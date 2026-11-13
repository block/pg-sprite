package checksum

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/decode"
)

// ConfirmedSource answers how far the catch-up has applied the stream: the
// position it last confirmed to the server, below which every change to a
// landed key is committed in the shadow. The applier's Catchup is one. An
// implementation of its own must answer from memory without blocking — a
// live pass reads it from its own goroutine once every LiveOptions.Poll
// while it waits — must never answer lower than it already has, and must
// follow the walsender's own position, not only the transactions it
// delivers: the pass waits for the position of a marker it wrote, which
// no transaction carries, and a source that advances only on applied
// transactions never reaches it on a quiet table.
type ConfirmedSource interface {
	Confirmed() decode.LSN
}

// FlushGate holds the catch-up's flushes off the shadow for as long as a
// live repair recopies it. The applier's Catchup is one. Pause returns
// once no flush is in flight, with none starting until resume is called,
// or with ctx's cause once ctx ends first; the catch-up goes on reading
// and confirming the stream under the hold, so what the source writes
// meanwhile is buffered and flushed after the resume. An implementation
// of its own must make the same promise: nothing the stream carries
// writes the shadow between Pause's return and the resume. The caller
// calls resume exactly once on every path after Pause returns a nil
// error, including when its own work fails, so an implementation need
// not tolerate a second call or a missing one.
type FlushGate interface {
	Pause(ctx context.Context) (resume func(), err error)
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
	// CauseHot is a chunk some key of which changed between every two of
	// LiveOptions.Attempts rereads, so the key was never read the same
	// way twice and the chunk could not be judged.
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
	// Keys is how many of the chunk's keys were still changing at the
	// last reread. It is zero under CauseLagging: a pass that never got
	// to reread the chunk judged no key.
	Keys int
	// HotKeys is the lowest of those keys, in ascending order, at most
	// hotKeysShown of them, so an operator can tell one row written over
	// and over from churn across the chunk. It is empty under
	// CauseLagging. The keys are the table's integer key values, which the
	// chunk bounds already name; no other column of a row is carried.
	HotKeys []int64
	// Source and Shadow are the two digests of the pass's last read.
	Source Digest
	Shadow Digest
	// Captured is the position of the marker the last read wrote to the
	// WAL; every change the read saw committed before it, and the pass was
	// waiting for the catch-up to confirm past it.
	Captured decode.LSN
	// Confirmed is the catch-up's confirmed position when the pass gave up.
	Confirmed decode.LSN
}

// Error says what the pass could not decide and what to do about it: a
// lagging catch-up wants the pass run again once it has caught up, and a
// hot chunk wants a quieter source.
func (e *UnresolvedError) Error() string {
	switch e.Cause {
	case CauseLagging:
		return fmt.Sprintf("chunk [%d, %d] read at %s is unsettled: the catch-up has confirmed only %s after %s; run the pass again once the catch-up has confirmed past the read",
			e.Chunk.Lower(), e.Chunk.Upper(), e.Captured, e.Confirmed, rereadsPhrase(e.Rereads))
	case CauseHot:
		return fmt.Sprintf("chunk [%d, %d] kept changing through %s: %s still changing%s, source %d rows, shadow %d rows at %s; the source is changing faster than the pass can compare it, so run the pass again when it is quieter",
			e.Chunk.Lower(), e.Chunk.Upper(), rereadsPhrase(e.Rereads), keysPhrase(e.Keys), hotKeysPhrase(e.HotKeys, e.Keys), e.Source.Rows, e.Shadow.Rows, e.Captured)
	default:
		return fmt.Sprintf("chunk [%d, %d] is unsettled after %s", e.Chunk.Lower(), e.Chunk.Upper(), rereadsPhrase(e.Rereads))
	}
}

// rereadsPhrase counts rereads in words that agree with the number.
func rereadsPhrase(n int) string {
	if n == 1 {
		return "1 reread"
	}
	return fmt.Sprintf("%d rereads", n)
}

// keysPhrase counts keys in words that agree with the number.
func keysPhrase(n int) string {
	if n == 1 {
		return "1 key"
	}
	return fmt.Sprintf("%d keys", n)
}

// hotKeysShown bounds how many still-changing keys an UnresolvedError
// carries and names, so a chunk with every key hot yields a short message.
const hotKeysShown = 3

// firstHotKeys returns the lowest still-changing keys an UnresolvedError
// carries: at most hotKeysShown of hot, which is in ascending order, in a
// slice of its own.
func firstHotKeys(hot []int64) []int64 {
	return slices.Clone(hot[:min(len(hot), hotKeysShown)])
}

// hotKeysPhrase names the still-changing keys in parentheses — every one
// when keys holds them all, the lowest otherwise — or nothing when there
// are none to name. It names at most hotKeysShown keys whatever keys holds.
func hotKeysPhrase(keys []int64, total int) string {
	keys = keys[:min(len(keys), hotKeysShown)]
	if len(keys) == 0 {
		return ""
	}
	names := make([]string, len(keys))
	for i, key := range keys {
		names[i] = strconv.FormatInt(key, 10)
	}
	label := "keys"
	switch {
	case total > len(keys):
		label = "lowest keys"
	case len(keys) == 1:
		label = "key"
	}
	return " (" + label + " " + strings.Join(names, ", ") + ")"
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
// take the defaults; Confirmed has none and is required, and so is
// Flushes under DivergenceRepair. Attempts and SettleWait bound each
// chunk, not the pass: a pass over many differing chunks can spend them on
// every one, so a caller with a deadline of its own passes it on the
// context.
type LiveOptions struct {
	// Confirmed is the catch-up applying the stream to the shadow.
	Confirmed ConfirmedSource
	// Flushes holds that catch-up's flushes off the shadow while a repair
	// recopies it. A pass under DivergenceRepair requires it; a pass under
	// DivergenceAbort recopies nothing and leaves it unused. The applier's
	// Catchup serves as both Confirmed and Flushes.
	Flushes FlushGate
	// Attempts is how many fresh rereads a chunk that keeps changing gets
	// before the pass reports it unsettled. Each reread reads the whole
	// chunk and counts once, however many of its keys it judges; a chunk
	// is read again only while some key of it is still changing, so a
	// chunk whose keys each hold still across one pair of reads settles
	// within the bound however much of the chunk is written.
	Attempts int
	// SettleWait bounds each wait for the catch-up to confirm past the
	// position a chunk was read at. It is spent in sleeps of Poll, so it
	// must be at least one Poll long.
	SettleWait time.Duration
	// Poll is the interval between reads of the catch-up's confirmed
	// position during a wait.
	Poll time.Duration
	// Sleep waits between polls; nil waits on the wall clock. Tests inject
	// one to drive the settling without real time passing.
	Sleep func(ctx context.Context, d time.Duration) error
}

// polls is how many sleeps of Poll a wait for the catch-up makes before
// it gives up: the whole of SettleWait, in Poll steps.
func (o LiveOptions) polls() int {
	return int(o.SettleWait / o.Poll)
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

// validate runs after withDefaults, so every field is set, and for the
// policy the pass runs under, since only a repair needs the flush gate.
func (o LiveOptions) validate(policy DivergencePolicy) error {
	if o.Confirmed == nil {
		return fmt.Errorf("%w: a live pass needs the catch-up's confirmed position", ErrInvalidOptions)
	}
	if policy == DivergenceRepair && o.Flushes == nil {
		return fmt.Errorf("%w: a live pass under %q needs the catch-up's flush gate (LiveOptions.Flushes) to hold its flushes while a repair recopies", ErrInvalidOptions, DivergenceRepair)
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
	if o.SettleWait < o.Poll {
		return fmt.Errorf("%w: settle wait %s is shorter than settle poll %s, so no poll would fit in it", ErrInvalidOptions, o.SettleWait, o.Poll)
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
// writes: the pass reads every row of the chunk on both sides in the
// chunk's transaction, hashed and keyed, writes a marker to the WAL in
// the same transaction, waits for the catch-up to confirm past it, reads
// the rows again in a fresh snapshot, and judges the two reads key by key
// (settle). A key whose two sides then agree, or whose shadow has reached
// exactly what the earlier read saw on the source, is settled; a key that
// held still on both sides across a confirmed position and still differs
// is a divergence, which concludes the chunk; a key that kept changing is
// reread, with only the keys still hot, up to LiveOptions.Attempts times
// in all, and a chunk with hot keys left after that is unsettled. A chunk
// every key of which settled counts as clean, however many writes hit it
// while the pass ran. The sweep's premise is that the stream is the
// shadow's only writer, so the shadow either lags the source or has
// caught up to it, and nothing else moves it.
//
// The sweep holds the rows of a few differing chunks at most: once it
// holds that many it settles them before it reads on, so the rows a pass
// holds are bounded by the chunk size, not by the table or by how much of
// it differs. Every differing chunk is settled before the pass decides
// anything, so one chunk that will not settle does not hide a divergence
// in another;
// the Report counts the unsettled chunks and lists the confirmed
// divergences. A clean pass — every chunk equal or settled — mints the
// proofs as Check does, marked as a live pass's. A pass with unsettled
// chunks and no divergence returns the first unsettled chunk as an
// *UnresolvedError: not a finding about the data, the shadow is as the pass
// found it, and the caller runs the pass again. Under DivergenceAbort a
// divergence is returned as a *DivergenceError with the shadow untouched,
// whether or not other chunks were unsettled. Under DivergenceRepair every
// divergent chunk is recopied from the source in one transaction, with
// the catch-up's flushes held off the shadow through LiveOptions.Flushes
// for as long as it runs and SHARE MODE on the shadow against any other
// writer; the pass resumes the flushes, waits for the catch-up to confirm
// past the recopy, and settles the chunk again; a chunk divergent after
// its repair is a *RepairError, and
// a pass whose repairs all took but which left other chunks unsettled
// returns the first of them as an *UnresolvedError with the repairs
// committed and listed in the Outcome. A pass with repairs mints no proof
// (CO-2). DivergenceRecopy is refused: it is the reconcile mode's policy
// for a pass that reads nothing again, and a live pass settles every chunk
// it recopies.
//
// While it runs the verifier is the tracker's work source: the sweep's
// differing chunks count as chunks_mismatched, and every settling reread
// as chunks_reread, with the rows of every row-by-row read of a differing
// chunk, the sweep's and each reread's, as settle_rows_read. A CheckLive
// that overlaps a pass still running on this verifier is refused with
// ErrPassRunning.
func (v *Verifier) CheckLive(ctx context.Context, pool *pgxpool.Pool, through copier.Watermark, policy DivergencePolicy, live LiveOptions) (Outcome, error) {
	if err := policy.validate(); err != nil {
		return Outcome{}, v.verifyError(err)
	}
	// INV: CO-3
	if policy == DivergenceRecopy {
		return Outcome{}, v.verifyError(fmt.Errorf("%w: %q reads nothing again; a live pass settles every chunk it recopies, so it takes %q or %q", ErrNoDivergencePolicy, DivergenceRecopy, DivergenceAbort, DivergenceRepair))
	}
	live = live.withDefaults()
	if err := live.validate(policy); err != nil {
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
	report, unresolved, err := v.sweepAndSettle(ctx, pool, through, live)
	if err != nil {
		return Outcome{}, v.lostOr(err)
	}
	if report.Clean() {
		return v.prove(report, true), nil
	}
	if len(report.Mismatches) == 0 {
		return Outcome{Report: report}, v.verifyError(unresolved)
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
		if unresolved != nil {
			return Outcome{Report: report, Repairs: repairs}, v.verifyError(unresolved)
		}
		return Outcome{Report: report, Repairs: repairs}, nil
	default:
		return Outcome{Report: report}, fmt.Errorf("%w (CO-3): divergence policy %q passed validation", ErrInvariantViolation, string(policy))
	}
}

// heldChunks is how many differing chunks a live sweep holds the rows of
// before it settles them and reads on. Every held chunk keeps a key and a
// hash per row on both sides, and no chunk is longer than the chunker's
// maximum, so however many chunks of a table differ, the rows a pass holds
// are bounded by this many chunks. Holding more than one lets the catch-up
// confirm past the earlier chunks' markers while the sweep reads the later
// ones, so a held chunk need not wait for the catch-up on its own.
const heldChunks = 4

// sweepAndSettle runs the live sweep and settles every chunk it found
// differing, in key order, returning the report with the confirmed
// divergences as its mismatches and the settled and unsettled chunks
// counted, and the first unsettled chunk when there was one. The sweep
// holds the chunks it finds differing until heldChunks of them are held,
// settles those, and reads on; whatever it holds when it reaches the
// watermark is settled last. A chunk that does not settle is counted and
// the next is settled; only a failure that says nothing about the data —
// the context ending, the lock lost, a read refused — stops the settling.
func (v *Verifier) sweepAndSettle(ctx context.Context, pool *pgxpool.Pool, through copier.Watermark, live LiveOptions) (Report, *UnresolvedError, error) {
	stmts, err := v.digestStatements(ctx, pool)
	if err != nil {
		return Report{}, nil, err
	}
	var (
		held    []observation
		settled settledChunks
	)
	settleHeld := func(ctx context.Context) error {
		for _, prior := range held {
			if err := v.settleInto(ctx, pool, stmts, prior, live, &settled); err != nil {
				return err
			}
		}
		held = nil
		return nil
	}
	report, err := v.sweep(ctx, pool, stmts, through, func(ctx context.Context, differing observation) error {
		held = append(held, differing)
		if len(held) < heldChunks {
			return nil
		}
		return settleHeld(ctx)
	})
	if err != nil {
		return Report{}, nil, err
	}
	if err := settleHeld(ctx); err != nil {
		return Report{}, nil, err
	}
	if lost := v.lockLost(); lost != nil {
		return Report{}, nil, lost
	}
	report.Mismatches = settled.mismatches
	report.Settled = settled.settled
	report.Unsettled = settled.unsettled
	return report, settled.first, nil
}

// settledChunks is what a live pass has concluded about the chunks it has
// settled so far: the confirmed divergences in key order, how many chunks
// settled and how many could not, and the first that could not.
type settledChunks struct {
	mismatches []Mismatch
	settled    int
	unsettled  int
	first      *UnresolvedError
}

// settleInto settles one differing chunk and records what it concluded in
// into. An unsettled chunk is recorded, not returned: only a failure that
// says nothing about the data is an error here.
func (v *Verifier) settleInto(ctx context.Context, pool *pgxpool.Pool, stmts statements, prior observation, live LiveOptions, into *settledChunks) error {
	settled, err := v.settle(ctx, pool, stmts, prior, 0, live)
	if unresolved, ok := errors.AsType[*UnresolvedError](err); ok {
		into.unsettled++
		if into.first == nil {
			into.first = unresolved
		}
		return nil
	}
	if err != nil {
		return err
	}
	if settled.diverged > 0 {
		into.mismatches = append(into.mismatches, settled.last.mismatch())
		return nil
	}
	into.settled++
	return nil
}
