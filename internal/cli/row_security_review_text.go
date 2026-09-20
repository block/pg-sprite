package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/block/pg-sprite/pkg/diffplan"
	"github.com/block/pg-sprite/pkg/schemadiff"
)

func writeRowSecurityReview(out io.Writer, review *diffplan.RowSecurityReviewRequired) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%s.%s — row security review\n", review.Schema, review.Table)
	for _, change := range review.Review.Changes {
		if change.BeforeSetting != nil {
			fmt.Fprintf(&b, "  %s: %t → %t [%s]\n", change.Kind, *change.BeforeSetting, *change.AfterSetting, change.Impact)
		} else {
			fmt.Fprintf(&b, "  %s %q [%s]\n", change.Kind, change.Policy, change.Impact)
			renderPolicySnapshot(&b, "before", change.BeforePolicy)
			renderPolicySnapshot(&b, "after", change.AfterPolicy)
		}
	}
	if review.Review.TableChanged {
		b.WriteString("  Table changes also present; the entire change remains blocked.\n")
	}
	if !review.Review.TableComparisonComplete {
		fmt.Fprintf(&b, "  Table comparison incomplete: %s\n", review.Review.TableComparisonError)
	}
	b.WriteString("Access impact is advisory; grants, role membership, and helper bodies are not compared.\n")
	if _, err := io.WriteString(out, b.String()); err != nil {
		return fmt.Errorf("write RLS review: %w", err)
	}
	return nil
}

func renderPolicySnapshot(b *strings.Builder, label string, p *schemadiff.PolicySnapshot) {
	if p == nil {
		fmt.Fprintf(b, "    %s: absent\n", label)
		return
	}
	fmt.Fprintf(b, "    %s: command=%s permissive=%t roles=%q\n", label, policyCommandText(p.Command), p.Permissive, p.Roles)
	fmt.Fprintf(b, "      USING: %s\n      WITH CHECK: %s\n      COMMENT: %s\n", policyText(p.Using), policyText(p.WithCheck), policyText(p.Comment))
}

func policyText(s *string) string {
	if s == nil {
		return "(omitted)"
	}
	return fmt.Sprintf("%q", *s)
}

func policyCommandText(c schemadiff.PolicyCommand) string {
	switch c {
	case schemadiff.PolicyAll:
		return "ALL"
	case schemadiff.PolicySelect:
		return "SELECT"
	case schemadiff.PolicyInsert:
		return "INSERT"
	case schemadiff.PolicyUpdate:
		return "UPDATE"
	case schemadiff.PolicyDelete:
		return "DELETE"
	default:
		return string(c)
	}
}
