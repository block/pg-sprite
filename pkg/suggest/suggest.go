// Package suggest is the advisory surface: it maps DDL that is risky as
// written to the safer native form the engine would run instead, offline
// and without executing anything. Every safer-idiom decision yields a
// suggestion — a constructed rewrite carries the safer sequence with typed
// caveats, because a safer form is not a semantic equivalent: it reaches
// the same end state with different locking, transactionality, and failure
// modes; an operation whose rewrite the planner cannot construct carries
// typed guidance naming the manual path instead of staying silent.
// Refusals, table rewrites, and destructive drops are pkg/lint's job.
package suggest

import (
	"fmt"

	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/statement"
)

// FormatVersion identifies the report contract
// (docs/suggest-report.md). A consumer must reject a report whose version
// it does not understand instead of guessing at the field semantics.
// Version 2 added name-constraint-then-validate and
// unique-index-then-constraint to the Guidance vocabulary.
const FormatVersion = 2

// Caveat is a typed condition attached to a recommendation; automation
// branches on it, never on prose. The caveats are independent — no caveat
// implies another; a sequence carries every caveat that applies to it.
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
	// CaveatScaffoldConstraintOnFailure: a failed VALIDATE leaves the
	// NOT VALID constraint the sequence added on the live table, and
	// replaying the sequence then fails at the ADD CONSTRAINT step
	// (duplicate_object) — the runner must detect the leftover constraint
	// (pg_constraint) and resume from the VALIDATE step, or drop it and
	// restart.
	CaveatScaffoldConstraintOnFailure Caveat = "scaffold-constraint-on-failure"
)

// Caveats returns the closed set of Caveat values. It is part of the
// suggest-report contract (docs/suggest-report.md): the set changes only
// with a format_version bump, and a consumer that meets an unrecognized
// value must treat the recommendation as unknown and refuse to run it.
func Caveats() []Caveat {
	return []Caveat{
		CaveatNonTransactional,
		CaveatSeparateTransactions,
		CaveatInvalidIndexOnFailure,
		CaveatDetachFinalizeOnFailure,
		CaveatValidationScan,
		CaveatScaffoldConstraintOnFailure,
	}
}

// Guidance is the typed manual path for a risky operation whose safer form
// the planner cannot construct; automation branches on it, never on prose.
// It is what keeps the advisory surface aligned with pkg/lint: every
// statement lint flags blocking-idiom gets advice here — a constructed
// rewrite or, failing that, guidance.
type Guidance string

// The guidance codes a suggestion can carry.
const (
	// GuidanceSplitStatement: rewrites are constructed only for
	// single-operation statements — a partial rewrite of a compound ALTER
	// would be misleading. Split the statement into one operation per
	// statement and advise again.
	GuidanceSplitStatement Guidance = "split-statement"
	// GuidanceAddColumnThenConstraint: an inline UNIQUE / PRIMARY KEY /
	// FOREIGN KEY / CHECK on ADD COLUMN builds or validates under the ADD
	// COLUMN's ACCESS EXCLUSIVE lock. Add the plain column first, then
	// build the constraint with its online pattern as a separate, named
	// ADD CONSTRAINT — named, because the unnamed ADD CHECK / ADD FOREIGN
	// KEY forms are themselves refused (GuidanceNameConstraintThenValidate).
	GuidanceAddColumnThenConstraint Guidance = "add-column-then-constraint"
	// GuidancePrevalidatedCheck: ATTACH PARTITION scans the child under
	// the parent's lock unless a validated CHECK matching the partition
	// bound already exists on the child. Pre-add that CHECK (NOT VALID,
	// then VALIDATE), attach, then drop it. The planner cannot construct
	// the bound-matching CHECK from the statement alone.
	GuidancePrevalidatedCheck Guidance = "pre-add-validated-check"
	// GuidanceNotNullScaffold: prove the invariant with a NOT VALID CHECK
	// (col IS NOT NULL) plus an online VALIDATE, then the NOT NULL
	// constraint is a catalog flip — the same scaffold sequence the
	// SET NOT NULL form gets constructed.
	GuidanceNotNullScaffold Guidance = "not-null-scaffold"
	// GuidanceNameConstraintThenValidate: an unnamed ADD CHECK / ADD
	// FOREIGN KEY has no constructible rewrite because the online
	// sequence's VALIDATE CONSTRAINT step needs the constraint's name and
	// the server assigns one only at creation. Name the constraint, add
	// it NOT VALID, then VALIDATE it online.
	GuidanceNameConstraintThenValidate Guidance = "name-constraint-then-validate"
	// GuidanceUniqueIndexThenConstraint: an ADD PRIMARY KEY / ADD UNIQUE
	// whose USING INDEX rewrite could not be constructed. Build the unique
	// index with CREATE UNIQUE INDEX CONCURRENTLY, then attach it with
	// ADD CONSTRAINT … USING INDEX — the same sequence the constructed
	// rewrite emits. Keeps ManualGuidance total over every constraint kind
	// the classifier can mark safer-idiom, so a parser shape that yields
	// no rewrite becomes advice rather than a failed report.
	GuidanceUniqueIndexThenConstraint Guidance = "unique-index-then-constraint"
)

// Guidances returns the closed set of Guidance values. It is part of the
// suggest-report contract (docs/suggest-report.md): the set changes only
// with a format_version bump, and a consumer that meets an unrecognized
// value must surface the suggestion as unknown rather than ignore it.
func Guidances() []Guidance {
	return []Guidance{
		GuidanceSplitStatement,
		GuidanceAddColumnThenConstraint,
		GuidancePrevalidatedCheck,
		GuidanceNotNullScaffold,
		GuidanceNameConstraintThenValidate,
		GuidanceUniqueIndexThenConstraint,
	}
}

// Suggestion is one advisory result: the statement as written and either
// the safer native sequence with its typed metadata, or typed guidance
// naming the manual path when no sequence could be constructed.
type Suggestion struct {
	// Statement is the 1-based index of the statement in the script.
	Statement int `json:"statement"`
	// Line is the 1-based source line of the statement's first token, so
	// a consumer can annotate the advice onto the file it came from.
	Line int `json:"line"`
	// Column is the 1-based source column of the statement's first token.
	Column int `json:"column"`
	// Original is the statement's verbatim source text (without the
	// trailing semicolon), so it can be found in the source by exact
	// match.
	Original string `json:"original"`
	// Operation is the operator-facing label of the risky operation
	// (display only).
	Operation string `json:"operation"`
	// Reason is the classifier's typed cause for preferring the rewrite.
	Reason planner.Reason `json:"reason"`
	// Recommended is the ordered safer SQL to run instead, present exactly
	// when the planner constructed the rewrite. Absent, Guidance names the
	// manual path.
	Recommended []string `json:"recommended,omitempty"`
	// Execution is the typed execution contract for Recommended
	// (planner.Execution), present exactly when Recommended is. A consumer
	// that runs the sequence branches on it instead of prose — it is what
	// says the steps must never be wrapped in one transaction block.
	Execution planner.Execution `json:"execution,omitempty"`
	// Caveats are the typed conditions under which the recommendation
	// differs from the original, present exactly when Recommended is and
	// never empty — a rewrite with no trade would be the same statement.
	Caveats []Caveat `json:"caveats,omitempty"`
	// Guidance is the typed manual path, present exactly when Recommended
	// is absent: the submitted form still blocks, and this names what to
	// do about it.
	Guidance Guidance `json:"guidance,omitempty"`
}

// Report is the advisory result for one script.
type Report struct {
	// FormatVersion is the report contract version; always FormatVersion.
	FormatVersion int `json:"format_version"`
	// Suggestions are the advisory results in statement order, one per
	// safer-idiom decision; empty means every statement is already in its
	// safest known form or is outside the advisory surface (refusals and
	// rewrites are lint findings).
	Suggestions []Suggestion `json:"suggestions"`
}

// Advise maps a DDL script to its advisory results: every statement is
// parsed with the PostgreSQL grammar and classified with zero live facts,
// and each risky-as-written operation yields a Suggestion — the safer
// sequence when the planner could construct it, typed guidance when it
// could not. Nothing is executed and no database is touched. A parse
// failure is an error.
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
// safer-idiom decision. The operation list and decision list are
// index-aligned by the planner's contract (one decision per operation, in
// order); a mismatch is a contract violation and fails closed.
func adviseStatement(index int, stmt statement.SourceStatement) ([]Suggestion, error) {
	plan, err := planner.Classify(stmt.SQL, planner.Facts{})
	if err != nil {
		return nil, err
	}
	ops, err := statement.ParseOps(stmt.SQL)
	if err != nil {
		return nil, err
	}
	if len(ops) != len(plan.Decisions) {
		return nil, fmt.Errorf("planner produced %d decisions for %d operations", len(plan.Decisions), len(ops))
	}
	var suggestions []Suggestion
	for i, d := range plan.Decisions {
		if d.Reason != planner.ReasonSaferIdiom {
			continue
		}
		s := Suggestion{
			Statement: index,
			Line:      stmt.Line,
			Column:    stmt.Column,
			Original:  stmt.SQL,
			Operation: d.Operation,
			Reason:    d.Reason,
		}
		if len(d.SaferSQL) > 0 {
			caveats, err := rewriteCaveats(ops[i])
			if err != nil {
				return nil, err
			}
			s.Recommended, s.Execution, s.Caveats = d.SaferSQL, d.SaferSQLExecution, caveats
		} else {
			guidance, err := ManualGuidance(ops[i], len(ops) > 1)
			if err != nil {
				return nil, err
			}
			s.Guidance = guidance
		}
		suggestions = append(suggestions, s)
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
		return []Caveat{CaveatSeparateTransactions, CaveatValidationScan, CaveatScaffoldConstraintOnFailure}, nil
	case statement.OpAddConstraint:
		switch op.Constraint {
		case statement.ConstraintPrimaryKey, statement.ConstraintUnique:
			// The concurrent index build and the USING INDEX attach must
			// also commit separately — non-transactional does not imply it.
			return []Caveat{CaveatNonTransactional, CaveatSeparateTransactions, CaveatInvalidIndexOnFailure}, nil
		case statement.ConstraintCheck, statement.ConstraintForeignKey:
			return []Caveat{CaveatSeparateTransactions, CaveatValidationScan, CaveatScaffoldConstraintOnFailure}, nil
		}
	}
	return nil, fmt.Errorf("no caveat mapping for rewritten operation %q", op.Describe())
}

// ManualGuidance maps a safer-idiom operation without a constructed
// rewrite to the typed manual path; multi says the operation arrived in a
// multi-operation statement, which always advises splitting first. The
// plan report derives its rewrite-required guidance through this same
// function so the two surfaces can never disagree. A safer-idiom decision
// this table does not know is a contract violation — when the planner
// learns a new non-constructible pattern, its guidance must be recorded
// here before the advice ships — so it fails closed rather than staying
// silent about a statement lint flags.
func ManualGuidance(op statement.Op, multi bool) (Guidance, error) {
	if multi {
		return GuidanceSplitStatement, nil
	}
	switch op.Kind {
	case statement.OpAddColumn:
		return GuidanceAddColumnThenConstraint, nil
	case statement.OpAttachPartition:
		return GuidancePrevalidatedCheck, nil
	case statement.OpAddConstraint:
		switch op.Constraint {
		case statement.ConstraintNotNull:
			return GuidanceNotNullScaffold, nil
		case statement.ConstraintCheck, statement.ConstraintForeignKey:
			// Reached only for the unnamed form: a named CHECK / FOREIGN
			// KEY gets the NOT VALID → VALIDATE rewrite constructed.
			return GuidanceNameConstraintThenValidate, nil
		case statement.ConstraintPrimaryKey, statement.ConstraintUnique:
			// Reached only when the USING INDEX rewrite could not be
			// constructed from the statement; covering the kind keeps
			// this mapping total over every constraint kind the
			// classifier marks safer-idiom, so the miss surfaces as
			// advice rather than a failed report.
			return GuidanceUniqueIndexThenConstraint, nil
		}
	}
	return "", fmt.Errorf("no guidance mapping for non-constructible operation %q", op.Describe())
}
