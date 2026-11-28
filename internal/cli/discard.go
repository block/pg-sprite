package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/decode"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/schemachange"
	"github.com/block/pg-sprite/pkg/verdict"
)

// discardFormatVersion identifies the discard report's JSON shape. It is
// bumped whenever a field is added, removed, or changes meaning; a consumer
// must reject a version it does not know.
const discardFormatVersion = 1

// discardTimeout bounds one discard. Dropping a stopped live run's slot
// waits for any server process still holding it, such as the walsender of
// a run whose host died, which the server ends once wal_sender_timeout
// passes; the bound leaves room for that wait at the server's default and
// keeps a slot held for longer from holding the command forever.
const discardTimeout = 5 * time.Minute

// discardOutcome is how one discard ended.
type discardOutcome string

const (
	// discardOutcomeDiscarded: the stopped run's leftovers were dropped.
	discardOutcomeDiscarded discardOutcome = "discarded"
	// discardOutcomeNothing: the table has no stopped run and no leftover
	// under its names; nothing was dropped and the command exits 0.
	discardOutcomeNothing discardOutcome = "nothing-to-discard"
	// discardOutcomeRefused: the discard was refused before it dropped
	// anything; the command exits 2.
	discardOutcomeRefused discardOutcome = "refused"
	// discardOutcomeStopped: the discard failed after it dropped part of
	// what the run left; the command exits 1, and a second discard picks
	// up what is left.
	discardOutcomeStopped discardOutcome = "stopped"
)

// Refusal causes the discard report names that are not the engine's own
// shadow refusal causes, which it reports as they are.
const (
	// discardCauseTableLockHeld: a run in progress, or another discard,
	// holds the table's lock.
	discardCauseTableLockHeld = "table-lock-held"
	// discardCauseSlotActive: a process holds the replication slot of the
	// table's derived name that no checkpoint row records.
	discardCauseSlotActive = "slot-active"
	// discardCauseTableLockUnconfirmed: the table lock session runs as a
	// role that cannot read when its own session started, so the lock was
	// never taken. It is the token migrate's verdict carries for the same
	// refusal.
	discardCauseTableLockUnconfirmed = string(executor.CodeTableLockUnconfirmed)
)

// errDiscardTableAmbiguous reports a SCHEMA.TABLE name that names a table
// at more than one of its dots.
var errDiscardTableAmbiguous = errors.New("the name names more than one table, splitting at different dots")

// discardReport is the discard command's output. Every name field is empty,
// and write_fence false, for a leftover the discard did not drop.
type discardReport struct {
	FormatVersion int            `json:"format_version"`
	Table         string         `json:"table"`
	Outcome       discardOutcome `json:"outcome"`
	// Cause is the typed refusal cause of a refused or stopped discard,
	// empty otherwise and for a stop that was not a refusal.
	Cause string `json:"cause"`
	// Detail is the error behind a refused or stopped discard.
	Detail string `json:"detail"`
	// RowPhase is the phase of the checkpoint row deleted.
	RowPhase string `json:"row_phase"`
	// Shadow is the shadow table dropped, in the table's schema.
	Shadow      string `json:"shadow"`
	Slot        string `json:"slot"`
	Publication string `json:"publication"`
	WriteFence  bool   `json:"write_fence"`
	// PublicationLeft names the publication a stopped discard left after
	// no replication slot of its name remained: the slot is gone, whether
	// this discard dropped it or it was gone already, and the publication
	// was not dropped.
	PublicationLeft string `json:"publication_left"`

	// schema is the table's schema, for the text's shadow name.
	schema string
}

// newDiscardReport fills the report's leftovers from what the discard
// dropped.
func newDiscardReport(table string, outcome discardOutcome, discarded schemachange.Discarded) discardReport {
	report := discardReport{
		FormatVersion: discardFormatVersion,
		Table:         table,
		Outcome:       outcome,
		Shadow:        discarded.Shadow,
		Slot:          discarded.Slot,
		Publication:   discarded.Publication,
		WriteFence:    discarded.WriteFence,
		schema:        discarded.Schema,
	}
	if discarded.RowPhase != 0 {
		report.RowPhase = discarded.RowPhase.String()
	}
	return report
}

// run discards the stopped copy-and-swap run on the named table through
// schemachange.Runner.Discard, under the shape proof the discard takes. A
// refusal before anything was dropped prints a refused report and returns
// verdict.ErrRefused; a discard that fails after dropping part of what the
// run left prints a stopped report and returns the error. A failure before
// any drop that is not a refusal, such as a connection failure, prints
// nothing.
func (c *DiscardCmd) run(ctx context.Context, out io.Writer) error {
	if !strings.Contains(c.Table, ".") {
		return fmt.Errorf("discard needs the table as SCHEMA.TABLE, as --restart names it; got %q", c.Table)
	}
	cfg := c.Config()
	pool, err := dbconn.NewPool(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()
	schema, table, err := resolveDiscardTable(ctx, pool, c.Table)
	if err != nil {
		return err
	}

	shape, replicationErr, err := proveDiscardShape(ctx, pool, schema, table)
	if err != nil {
		cause, ok := discardPreflightRefusal(err)
		if !ok {
			return fmt.Errorf("discard %s: %w", c.Table, err)
		}
		report := newDiscardReport(c.Table, discardOutcomeRefused, schemachange.Discarded{})
		report.Cause, report.Detail = cause, err.Error()
		return c.finish(out, report, verdict.ErrRefused)
	}
	store, err := checkpoint.NewStore(pool, checkpoint.Options{})
	if err != nil {
		return err
	}
	runner, err := schemachange.NewRunner(cfg, store, schemachange.RunOptions{})
	if err != nil {
		return err
	}
	c.audit().Warn("discard requested; whatever a stopped copy-and-swap run on the table left will be dropped and its change not run",
		"schema", schema, "table", table)
	discardCtx, cancel := context.WithTimeout(ctx, discardTimeout)
	defer cancel()
	discarded, err := runner.Discard(discardCtx, pool, shape)
	if err == nil {
		outcome := discardOutcomeDiscarded
		if discarded.Nothing() {
			outcome = discardOutcomeNothing
		}
		return c.finish(out, newDiscardReport(c.Table, outcome, discarded), nil)
	}
	if errors.Is(err, preflight.ErrCopySwapProofMismatch) && replicationErr != nil {
		// The run holds a slot, and dropping it needs the replication
		// access the role lacks: the access check's own refusal says
		// which grant gives it.
		err = fmt.Errorf("%w: %w", err, replicationErr)
	}
	outcome, cause := discardFailure(err, discarded)
	switch outcome {
	case discardOutcomeRefused:
		report := newDiscardReport(c.Table, outcome, discarded)
		report.Cause, report.Detail = cause, err.Error()
		return c.finish(out, report, verdict.ErrRefused)
	case discardOutcomeStopped:
		report := newDiscardReport(c.Table, outcome, discarded)
		report.Cause, report.Detail = cause, err.Error()
		if errors.Is(err, decode.ErrSlotDroppedPublicationLeft) {
			report.PublicationLeft = shape.DecodingName()
		}
		return c.finish(out, report, fmt.Errorf("discard %s: %w", c.Table, err))
	default:
		return fmt.Errorf("discard %s: %w", c.Table, err)
	}
}

// discardFailure sorts a discard that returned err after dropping
// discarded into the outcome its report states, with the refusal cause
// err carries: refused when a refusal came before anything was dropped,
// stopped when something is gone, and no outcome, which prints no report,
// for a failure before anything was dropped that is not a refusal. A
// publication left after its slot is a stop even when the call names
// nothing it dropped, since the slot of its name is gone.
func discardFailure(err error, discarded schemachange.Discarded) (discardOutcome, string) {
	cause, refused := discardRefusal(err)
	if errors.Is(err, decode.ErrSlotDroppedPublicationLeft) {
		return discardOutcomeStopped, cause
	}
	if !discarded.Nothing() {
		return discardOutcomeStopped, cause
	}
	if refused {
		return discardOutcomeRefused, cause
	}
	return "", ""
}

// finish prints the report and returns result, or the write's error
// alongside it.
func (c *DiscardCmd) finish(out io.Writer, report discardReport, result error) error {
	if err := c.write(out, report); err != nil {
		return errors.Join(result, err)
	}
	return result
}

// discardTableSQL reports whether an ordinary table bears the schema and
// table names given. Copy-and-swap runs only on ordinary tables, so no
// other relation has a run to discard. Every catalog name is qualified
// (CO-9).
const discardTableSQL = `
SELECT EXISTS (
	SELECT 1
	FROM pg_catalog.pg_class c
	JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
	WHERE n.nspname OPERATOR(pg_catalog.=) $1
	  AND c.relname OPERATOR(pg_catalog.=) $2
	  AND c.relkind OPERATOR(pg_catalog.=) 'r'
)`

// resolveDiscardTable finds the table named in the engine's flat
// SCHEMA.TABLE spelling, the one --restart compares against: the schema
// and table names joined by a dot, with no quoting. A name may itself hold
// a dot, so every dot is tried as the split, and exactly one split must
// name a table.
func resolveDiscardTable(ctx context.Context, pool *pgxpool.Pool, flat string) (schema, table string, err error) {
	var found [][2]string
	for i := range len(flat) {
		if flat[i] != '.' {
			continue
		}
		s, t := flat[:i], flat[i+1:]
		var exists bool
		if err := pool.QueryRow(ctx, discardTableSQL, s, t).Scan(&exists); err != nil {
			return "", "", fmt.Errorf("look up table %s: %w", flat, err)
		}
		if exists {
			found = append(found, [2]string{s, t})
		}
	}
	switch len(found) {
	case 0:
		return "", "", fmt.Errorf("%w: no table is named %s; name it as --restart does, the schema-qualified name with an unquoted name in lower case",
			preflight.ErrTableNotFound, flat)
	case 1:
		return found[0][0], found[0][1], nil
	default:
		return "", "", fmt.Errorf("%s: %w; nothing was discarded", flat, errDiscardTableAmbiguous)
	}
}

// proveDiscardShape proves the table for the discard: the copy-and-swap
// privileges and shape, with no environment check, since a discard grows
// nothing. Replication access is proven when the role has it, since
// dropping a live run's slot needs it; a role without it can still discard
// an offline run, and the discard refuses a slot drop under a proof without
// it before dropping anything. replicationErr is the access check's
// refusal of the role's replication access, nil when the proof covers it.
func proveDiscardShape(ctx context.Context, pool *pgxpool.Pool, schema, table string) (shape preflight.CopySwapShape, replicationErr, err error) {
	role, err := preflight.CheckPrivileges(ctx, pool, schema, table,
		preflight.Requirement{Tier: preflight.TierCopyAndSwap, LogicalDecoding: true})
	if _, ok := errors.AsType[*preflight.PrivilegeError](err); ok {
		replicationErr = err
		role, err = preflight.CheckPrivileges(ctx, pool, schema, table,
			preflight.Requirement{Tier: preflight.TierCopyAndSwap})
	}
	if err != nil {
		return preflight.CopySwapShape{}, nil, err
	}
	shape, err = preflight.CheckCopySwapShape(ctx, pool, schema, table, role)
	if err != nil {
		return preflight.CopySwapShape{}, nil, err
	}
	return shape, replicationErr, nil
}

// discardPreflightRefusal sorts an error from proving the table into the
// refusal cause it stands for: a missing privilege, or a table shape the
// copy-and-swap route does not take. Anything else is not a refusal.
func discardPreflightRefusal(err error) (string, bool) {
	if _, ok := errors.AsType[*preflight.PrivilegeError](err); ok {
		return string(verdict.ReasonInsufficientPrivileges), true
	}
	if cause := preflight.CopySwapRefusalCauseOf(err); cause != "" {
		return string(cause), true
	}
	return "", false
}

// discardRefusal sorts an error from the discard into the refusal cause it
// stands for. A shadow refusal carries its own cause; a lock another
// session holds, a lock session that cannot read its own start, a held
// unrecorded slot, and a slot the role may not drop are refusals of their
// own. Anything else is not a refusal.
func discardRefusal(err error) (string, bool) {
	if cause := schemachange.RefusalCauseOf(err); cause != "" {
		return string(cause), true
	}
	if _, ok := errors.AsType[*dbconn.TableLockHeldError](err); ok {
		return discardCauseTableLockHeld, true
	}
	if errors.Is(err, dbconn.ErrSessionStartHidden) {
		return discardCauseTableLockUnconfirmed, true
	}
	if errors.Is(err, decode.ErrSlotActive) {
		return discardCauseSlotActive, true
	}
	if errors.Is(err, preflight.ErrCopySwapProofMismatch) {
		return string(verdict.ReasonInsufficientPrivileges), true
	}
	return "", false
}

// write prints the report as JSON or as text.
func (c *DiscardCmd) write(out io.Writer, report discardReport) error {
	if c.JSON {
		b, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return fmt.Errorf("encode discard report: %w", err)
		}
		if _, err := fmt.Fprintln(out, string(b)); err != nil {
			return fmt.Errorf("write discard report: %w", err)
		}
		return nil
	}
	if _, err := io.WriteString(out, renderDiscard(report)); err != nil {
		return fmt.Errorf("write discard report: %w", err)
	}
	return nil
}

// renderDiscard is the human-readable discard report: the outcome, then
// one line for each leftover dropped, in the order the discard drops them.
func renderDiscard(report discardReport) string {
	var b strings.Builder
	switch report.Outcome {
	case discardOutcomeDiscarded:
		fmt.Fprintf(&b, "discarded the stopped copy-and-swap run on %s; its change was not run\n", report.Table)
	case discardOutcomeNothing:
		fmt.Fprintf(&b, "nothing to discard on %s: no checkpoint row, shadow table, write fence, or replication slot of a copy-and-swap run\n", report.Table)
		return b.String()
	case discardOutcomeRefused:
		fmt.Fprintf(&b, "refused to discard the run on %s (%s): %s\nnothing was dropped\n", report.Table, report.Cause, report.Detail)
		return b.String()
	case discardOutcomeStopped:
		fmt.Fprintf(&b, "the discard on %s stopped partway: %s\n", report.Table, report.Detail)
	}
	writeDiscarded(&b, report)
	if report.Outcome == discardOutcomeStopped {
		fmt.Fprintf(&b, "run discard %s again to drop what is left\n", report.Table)
	}
	if report.PublicationLeft != "" {
		fmt.Fprintf(&b, "a publication %s that discard leaves again is not of the engine's shape: drop it by hand once nothing uses it\n", report.PublicationLeft)
	}
	return b.String()
}

// writeDiscarded writes one line for each leftover the report names.
func writeDiscarded(b *strings.Builder, report discardReport) {
	if report.Shadow != "" {
		fmt.Fprintf(b, "  dropped shadow table %s.%s\n", report.schema, report.Shadow)
	}
	if report.WriteFence {
		fmt.Fprintf(b, "  took the write fence off %s: writes to it succeed again\n", report.Table)
	}
	if report.Slot != "" {
		fmt.Fprintf(b, "  dropped replication slot %s\n", report.Slot)
	}
	if report.Publication != "" {
		fmt.Fprintf(b, "  dropped publication %s\n", report.Publication)
	}
	if report.RowPhase != "" {
		fmt.Fprintf(b, "  deleted the checkpoint row (phase %s)\n", report.RowPhase)
	}
	if report.PublicationLeft != "" {
		fmt.Fprintf(b, "  no replication slot %s remains; its publication was left in place\n", report.PublicationLeft)
	}
}
