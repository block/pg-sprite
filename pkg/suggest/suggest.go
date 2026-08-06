// Package suggest is the advisory surface: it maps DDL that is risky as
// written to the safer native form the engine would run instead, offline
// and without executing anything. It reports only constructed rewrites —
// refusals, table rewrites, and destructive drops are pkg/lint's job — and
// every recommendation carries typed caveats, because a safer form is not
// a semantic equivalent: it reaches the same end state with different
// locking, transactionality, and failure modes.
package suggest

import (
	"fmt"

	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/statement"
)

// FormatVersion identifies the report contract. A consumer must reject a
// report whose version it does not understand instead of guessing at the
// field semantics.
const FormatVersion = 1

// Caveat is a typed condition attached to a recommendation; automation
// branches on it, never on prose.
type Caveat string

// The caveats a recommendation can carry.
const (
	// CaveatNonTransactional: the recommended sequence contains a
	// CONCURRENTLY statement, which cannot run inside a transaction
	// block.
	CaveatNonTransactional Caveat = "non-transactional"
	// CaveatSeparateTransactions: the steps must commit separately — the
	// weaker locks the sequence exists for are held to commit, so one
	// enclosing transaction reproduces the blocking the rewrite avoids.
	CaveatSeparateTransactions Caveat = "separate-transactions"
	// CaveatInvalidIndexOnFailure: a failed or cancelled concurrent build
	// leaves an INVALID index that must be detected (pg_index.indisvalid)
	// and dropped or rebuilt; the engine's executor owns that check when
	// it runs the sequence.
	CaveatInvalidIndexOnFailure Caveat = "invalid-index-on-failure"
	// CaveatDetachFinalizeOnFailure: an interrupted concurrent detach
	// leaves the partition half-detached; it must be finished with
	// DETACH PARTITION FINALIZE.
	CaveatDetachFinalizeOnFailure Caveat = "detach-finalize-on-failure"
	// CaveatValidationScan: the VALIDATE step still scans every row — the
	// rewrite trades the lock strength, not the scan.
	CaveatValidationScan Caveat = "validation-scan"
)

// Suggestion is one advisory rewrite: the statement as written, the safer
// native sequence, and the typed metadata explaining the trade.
type Suggestion struct {
	// Statement is the 1-based index of the statement in the script.
	Statement int `json:"statement"`
	// Original is the canonical text of the statement as submitted.
	Original string `json:"original"`
	// Operation is the operator-facing label of the risky operation
	// (display only).
	Operation string `json:"operation"`
	// Reason is the classifier's typed cause for preferring the rewrite.
	Reason planner.Reason `json:"reason"`
	// Recommended is the ordered safer SQL to run instead.
	Recommended []string `json:"recommended"`
	// Caveats are the typed conditions under which the recommendation
	// differs from the original; never empty — a rewrite with no trade
	// would be the same statement.
	Caveats []Caveat `json:"caveats"`
}

// Report is the advisory result for one script.
type Report struct {
	// FormatVersion is the report contract version; always FormatVersion.
	FormatVersion int `json:"format_version"`
	// Suggestions are the rewrites in statement order; empty means every
	// statement is already in its safest known form or is outside the
	// advisory surface (refusals and rewrites are lint findings).
	Suggestions []Suggestion `json:"suggestions"`
}

// Advise maps a DDL script to its advisory rewrites: every statement is
// parsed with the PostgreSQL grammar and classified with zero live facts,
// and each risky-as-written operation with a constructible safer form
// yields a Suggestion. Nothing is executed and no database is touched. A
// parse failure is an error.
func Advise(sql string) (Report, error) {
	stmts, err := statement.Split(sql)
	if err != nil {
		return Report{}, err
	}
	report := Report{FormatVersion: FormatVersion, Suggestions: []Suggestion{}}
	for i, stmt := range stmts {
		suggestions, err := adviseStatement(i+1, stmt)
		if err != nil {
			return Report{}, fmt.Errorf("statement %d: %w", i+1, err)
		}
		report.Suggestions = append(report.Suggestions, suggestions...)
	}
	return report, nil
}

// adviseStatement produces the suggestions for one statement: one per
// safer-idiom decision whose rewrite the planner could construct. The
// operation list and decision list are index-aligned by the planner's
// contract (one decision per operation, in order); a mismatch is a
// contract violation and fails closed.
func adviseStatement(index int, sql string) ([]Suggestion, error) {
	plan, err := planner.Classify(sql, planner.Facts{})
	if err != nil {
		return nil, err
	}
	ops, err := statement.ParseOps(sql)
	if err != nil {
		return nil, err
	}
	if len(ops) != len(plan.Decisions) {
		return nil, fmt.Errorf("planner produced %d decisions for %d operations", len(plan.Decisions), len(ops))
	}
	var suggestions []Suggestion
	for i, d := range plan.Decisions {
		if d.Reason != planner.ReasonSaferIdiom || len(d.SaferSQL) == 0 {
			continue
		}
		caveats, err := rewriteCaveats(ops[i])
		if err != nil {
			return nil, err
		}
		suggestions = append(suggestions, Suggestion{
			Statement:   index,
			Original:    sql,
			Operation:   d.Operation,
			Reason:      d.Reason,
			Recommended: d.SaferSQL,
			Caveats:     caveats,
		})
	}
	return suggestions, nil
}

// rewriteCaveats maps an operation to the typed caveats of its safer
// rewrite. An operation with a rewrite this table does not know is a
// contract violation — when the planner learns a new rewrite, its caveats
// must be recorded here before the advice ships — so it fails closed
// rather than emitting caveat-less advice.
func rewriteCaveats(op statement.Op) ([]Caveat, error) {
	switch op.Kind {
	case statement.OpCreateIndex, statement.OpDropIndex, statement.OpReindex:
		return []Caveat{CaveatNonTransactional, CaveatInvalidIndexOnFailure}, nil
	case statement.OpDetachPartition:
		return []Caveat{CaveatNonTransactional, CaveatDetachFinalizeOnFailure}, nil
	case statement.OpSetNotNull:
		return []Caveat{CaveatSeparateTransactions, CaveatValidationScan}, nil
	case statement.OpAddConstraint:
		switch op.Constraint {
		case statement.ConstraintPrimaryKey, statement.ConstraintUnique:
			return []Caveat{CaveatNonTransactional, CaveatInvalidIndexOnFailure}, nil
		case statement.ConstraintCheck, statement.ConstraintForeignKey:
			return []Caveat{CaveatSeparateTransactions, CaveatValidationScan}, nil
		}
	}
	return nil, fmt.Errorf("no caveat mapping for rewritten operation %q", op.Describe())
}
