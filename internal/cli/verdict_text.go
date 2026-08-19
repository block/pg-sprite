package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/block/pg-sprite/pkg/verdict"
)

// writeVerdictText renders a migrate verdict for humans with the outcome
// headline and field labels styled. The layout is verdict.String's — that
// method stays the plain source of truth for library callers — and the
// parity test pins the two renderings together byte for byte once escapes
// are stripped, so color can never change the verdict's shape and the two
// renderings cannot drift.
func writeVerdictText(out io.Writer, pal palette, v verdict.Verdict) error {
	var b strings.Builder
	b.WriteString(outcomeHeadline(pal, v))
	if v.Table != "" {
		fmt.Fprintf(&b, "\n  %s     %s", pal.bold("table:"), v.Table)
	}
	fmt.Fprintf(&b, "\n  %s %s", pal.bold("statement:"), v.Statement)
	if v.Attempts > 0 {
		fmt.Fprintf(&b, "\n  %s  %d", pal.bold("attempts:"), v.Attempts)
	}
	if v.Detail != "" {
		fmt.Fprintf(&b, "\n  %s    %s", pal.bold("detail:"), v.Detail)
	}
	if v.SaferIdiom != "" {
		fmt.Fprintf(&b, "\n  %s     %s", pal.bold("safer:"), v.SaferIdiom)
	}
	if v.Forced {
		fmt.Fprintf(&b, "\n  %s    the submitted form ran as-is (--force)", pal.bold("forced:"))
	}
	if v.FailedStep > 0 {
		fmt.Fprintf(&b, "\n  %s step %d: %s", pal.bold("failed at:"), v.FailedStep, v.FailedStepSQL)
	}
	if len(v.ExecutedSQL) > 0 {
		if v.Outcome == verdict.OutcomeFailed {
			fmt.Fprintf(&b, "\n  %s", pal.bold("committed before the failure (their state remains):"))
		} else {
			fmt.Fprintf(&b, "\n  %s", pal.bold("executed as:"))
		}
		for i, sql := range v.ExecutedSQL {
			fmt.Fprintf(&b, "\n    %d. %s", i+1, sql)
		}
	}
	if _, err := fmt.Fprintln(out, b.String()); err != nil {
		return fmt.Errorf("write verdict: %w", err)
	}
	return nil
}

// outcomeHeadline styles the verdict's first line by outcome: success is
// green like the diagnostic grammar's help, a refusal or failure is red
// like its errors, and an unknown outcome gets the structural bold so it
// is still visible, never invisible.
func outcomeHeadline(pal palette, v verdict.Verdict) string {
	switch v.Outcome {
	case verdict.OutcomeExecuted:
		return pal.severity("help", "executed natively")
	case verdict.OutcomeRefused:
		return pal.severity("error", fmt.Sprintf("refused (%s)", v.Reason))
	case verdict.OutcomeFailed:
		return pal.severity("error", fmt.Sprintf("failed (%s)", v.Code))
	}
	return pal.bold(fmt.Sprintf("unknown outcome %q", string(v.Outcome)))
}
