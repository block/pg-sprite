package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/block/pg-sprite/pkg/progress"
)

// progressFieldSeparator separates the fields of one progress line.
const progressFieldSeparator = "  "

// renderProgress renders one running snapshot as a single progress line:
// the step (only for a run of several), the operation, the server's phase
// for a concurrent index build, the percent, the step's counter, and the
// ETA. A field the snapshot omits is left out, so an unknown percent or ETA
// is never shown as zero. A snapshot that is not running renders nothing:
// before the run starts there is nothing to show, and once it ends the
// verdict reports the outcome.
func renderProgress(s progress.Snapshot) string {
	if s.Phase != progress.PhaseRunning {
		return ""
	}
	d := s.Detail
	var fields []string
	if s.TotalSteps > 1 {
		fields = append(fields, fmt.Sprintf("step %d/%d", s.Step, s.TotalSteps))
	}
	fields = append(fields, operationLabel(d.Operation))
	if d.ServerPhase != "" {
		fields = append(fields, d.ServerPhase)
	}
	if d.Percent != nil {
		fields = append(fields, fmt.Sprintf("%.1f%%", *d.Percent))
	}
	if counter := workCounter(d); counter != "" {
		fields = append(fields, counter)
	}
	if d.ETA != nil {
		fields = append(fields, "ETA "+formatETA(*d.ETA))
	}
	return strings.Join(fields, progressFieldSeparator)
}

// operationLabel is the operator-facing name of an operation. An operation
// this front door does not know yet is shown by its contract token rather
// than hidden.
func operationLabel(op progress.Operation) string {
	switch op {
	case "":
		return "starting"
	case progress.OperationAdmitting:
		return "admitting"
	case progress.OperationOptimistic, progress.OperationBrief:
		return "statement"
	case progress.OperationValidate:
		return "validate constraint"
	case progress.OperationConcurrentIndex:
		return "concurrent index build"
	case progress.OperationCopy:
		return "copy"
	case progress.OperationChecksum:
		return "checksum"
	case progress.OperationCatchUp:
		return "catch-up"
	}
	return string(op)
}

// workCounter renders the counter an operator reads next to the percent:
// rows for the copy and the checksum pass, the stream's lag for the
// catch-up. A concurrent index build's counters are already folded into its
// percent, and every other operation measures no work.
func workCounter(d progress.Detail) string {
	if d.Work == nil {
		return ""
	}
	switch d.Operation {
	case progress.OperationCopy:
		return rowCounter(d.Work.RowsCopied, d.Work.RowsTotal)
	case progress.OperationChecksum:
		return rowCounter(d.Work.RowsHashed, d.Work.RowsToHash)
	case progress.OperationCatchUp:
		return "lag " + formatBytes(d.Work.LagBytes)
	default:
		return ""
	}
}

// rowCounter renders done rows against the estimated total, or the done
// rows alone while no total is known.
func rowCounter(done, total uint64) string {
	if total == 0 {
		return formatCount(done) + " rows"
	}
	return formatCount(done) + "/" + formatCount(total) + " rows"
}

// formatCount abbreviates a count to at most one decimal of K, M, or B.
func formatCount(n uint64) string {
	switch {
	case n < 1_000:
		return fmt.Sprintf("%d", n)
	case n < 1_000_000:
		return fmt.Sprintf("%.1fK", float64(n)/1e3)
	case n < 1_000_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	default:
		return fmt.Sprintf("%.1fB", float64(n)/1e9)
	}
}

// formatBytes renders a size in the binary units the CLI's size flags take.
func formatBytes(n uint64) string {
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	v := float64(n) / 1024
	unit := 0
	for v >= 1024 && unit < len(units)-1 {
		v /= 1024
		unit++
	}
	return fmt.Sprintf("%.1f %s", v, units[unit])
}

// formatETA renders a duration to the second with its zero trailing units
// dropped: 3m10s, 1m, 40s, 2h5m.
func formatETA(d time.Duration) string {
	d = d.Round(time.Second)
	if d <= 0 {
		return "0s"
	}
	h, m, s := int(d/time.Hour), int(d%time.Hour/time.Minute), int(d%time.Minute/time.Second)
	var b strings.Builder
	if h > 0 {
		fmt.Fprintf(&b, "%dh", h)
	}
	if m > 0 {
		fmt.Fprintf(&b, "%dm", m)
	}
	if s > 0 {
		fmt.Fprintf(&b, "%ds", s)
	}
	return b.String()
}
