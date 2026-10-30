package migrate

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/suggest"
	"github.com/block/pg-sprite/pkg/verdict"
)

// This file is the imperative front door's destructive-change gate: a
// statement whose plan discards live data or structure runs only when the
// operator's acknowledgement names its table. The core never checks
// destructiveness (SAFETY.md), so this gate and the planner's Destructive
// derivation are the whole guard on this path.

var (
	// ErrAllowDestructiveMismatch means the destructive acknowledgement
	// named a table other than the statement's resolved target. Nothing has
	// executed.
	ErrAllowDestructiveMismatch = errors.New("the destructive acknowledgement must name the resolved target table")
	// ErrAllowDestructiveNotSupported is returned by [RunDesired] when
	// Options.AllowDestructive is set: desired-state execution refuses a
	// destructive plan whole and has no acknowledgement for it.
	ErrAllowDestructiveNotSupported = errors.New(
		"the destructive acknowledgement applies to the imperative front door only; desired-state execution refuses a destructive plan whole")
)

// checkAllowDestructiveAck validates the destructive acknowledgement
// against the resolved schema-qualified target table, the same typed
// acknowledgement the force override takes. A mismatch is a usage error:
// nothing has executed.
func checkAllowDestructiveAck(table, ack string) error {
	if ack == table {
		return nil
	}
	return fmt.Errorf("%w %q, got %q; nothing was executed", ErrAllowDestructiveMismatch, table, ack)
}

// destructiveOperations returns the labels of the routed statement's
// operations that would discard live data or structure, in statement order,
// for every disposition a route could run: executed as routed, or forced
// past a rewrite-required or backend-unavailable refusal. It is empty when
// the statement discards nothing, and for a planner refusal, which runs
// nothing and which no acknowledgement lifts, so that refusal keeps its own
// reason.
func destructiveOperations(rs router.Statement) []string {
	if rs.Disposition == router.DispositionRefuse {
		return nil
	}
	var ops []string
	for _, d := range rs.Decisions {
		if d.Destructive {
			ops = append(ops, d.Operation)
		}
	}
	return ops
}

// destructiveChangeVerdict is the refusal for a statement that discards
// live data or structure without the destructive acknowledgement. It shares
// its reason and class with the desired-state admission refusal, so both
// front doors report one contract. A statement that drops a column and adds
// it back under the same name carries the guidance to change the column in
// place, which keeps its values.
func destructiveChangeVerdict(st statement.Statement, ops []string) (verdict.Verdict, error) {
	detail := fmt.Sprintf("the statement discards live data or structure (%s); pg-sprite runs no destructive "+
		"statement unless the destructive acknowledgement names its table — review what is lost, then rerun "+
		"with --allow-destructive %s",
		strings.Join(ops, ", "), qualified(st))
	v := verdict.Verdict{Statement: st.SQL(), Table: qualified(st)}
	parsed, err := statement.ParseOps(st.SQL())
	if err != nil {
		return verdict.Verdict{}, fmt.Errorf("parse destructive statement on %s: %w", qualified(st), err)
	}
	if column, ok := suggest.ReAddedColumn(parsed); ok {
		v.Guidance = suggest.GuidanceAlterColumnTypeInPlace
		detail += fmt.Sprintf("; the statement drops %s and adds it back empty — to keep its values, %s",
			column, manualPath(v.Guidance))
	}
	v.Detail = detail
	return v.WithRefusal(destructiveChangeRefusal()), nil
}

// AdmitDestructive is the destructive-change gate a dry run applies to the
// statement it routed, with the result [Run] reaches for the same statement
// and acknowledgement: the refusal verdict when the routed statement rs
// discards live data or structure and ack is empty, or the mismatch error
// when ack does not name the resolved target table. The boolean is false
// when the statement discards nothing or ack names its table.
//
// schema is the schema st's table resolved to on the session's search_path,
// as the dry run's live facts read it; a qualified st ignores it. An
// unqualified st's verdict names the table and statement the way [Run]'s
// does, qualified.
func AdmitDestructive(st statement.Statement, schema string, rs router.Statement, ack string) (verdict.Verdict, bool, error) {
	unqualified := st.Schema() == "" && st.Table() != ""
	if unqualified && schema == "" {
		return verdict.Verdict{}, false, fmt.Errorf("admit destructive change for %s: %w", st.Table(), errAdmissionSchemaUnknown)
	}
	table := qualified(st)
	if unqualified {
		table = schema + "." + st.Table()
	}
	if ack != "" {
		return verdict.Verdict{}, false, checkAllowDestructiveAck(table, ack)
	}
	ops := destructiveOperations(rs)
	if len(ops) == 0 {
		return verdict.Verdict{}, false, nil
	}
	target := st
	if unqualified {
		resolved, err := resolveForVerdict(st, schema)
		if err != nil {
			return verdict.Verdict{}, false, err
		}
		target = resolved
	}
	v, err := destructiveChangeVerdict(target, ops)
	if err != nil {
		return verdict.Verdict{}, false, err
	}
	return v, true, nil
}

// dropIndexDestructiveVerdict is the refusal for an accepted blocking DROP
// INDEX without the destructive acknowledgement. Every index drop counts,
// not only a unique one: the common mistake is dropping an index the
// application still reads through, so the detail lists the checks that
// catch it before naming the acknowledgement. table is the index's owning
// table, resolved from the catalog.
func dropIndexDestructiveVerdict(st statement.Statement, table string) verdict.Verdict {
	detail := "DROP INDEX discards an index that queries, foreign-key checks, or a uniqueness guarantee may " +
		"still rely on, and undoing it is a full index build; before dropping, confirm that " +
		"pg_stat_user_indexes.idx_scan for the index is 0 on the primary and on every replica (each instance " +
		"keeps its own counters) over a window since stats_reset that covers the application's full cycle, " +
		"that no foreign key's referencing columns rely on it, and, for a unique index, that nothing still " +
		"needs the uniqueness it enforces — then rerun with --allow-destructive " + table
	v := verdict.Verdict{Statement: st.SQL(), Table: table, Detail: detail}
	return v.WithRefusal(destructiveChangeRefusal())
}

// auditAllowDestructive records the acknowledged data loss on table before
// anything executes. The record is warn-level and unconditional, as the
// force audit is, and the verdict's AllowedDestructive field is its
// machine-readable twin.
func auditAllowDestructive(audit *slog.Logger, st statement.Statement, table string) {
	audit.Warn("destructive change acknowledged; the statement will discard live data or structure",
		"table", table,
		"kind", st.Kind().String())
}
