// Package capabilities provides the embedded, validated support matrix and its Markdown renderer.
package capabilities

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/block/pg-sprite/pkg/verdict"
	"gopkg.in/yaml.v3"
)

//go:embed capabilities.yaml
var source []byte

// Area identifies one matrix table.
type Area string

// Tier describes whether a capability is supported, planned, or out of scope.
type Tier string

// StatusMark is the human-readable mark associated with a tier.
type StatusMark string

// EnginePath identifies how the engine executes or plans to execute an operation.
type EnginePath string

// FrontDoorStatus describes admission through one CLI front door.
type FrontDoorStatus string

// Closed values used by capability rows.
const (
	AreaColumnChanges               Area            = "column_changes"
	AreaConstraints                 Area            = "constraints"
	AreaIndexes                     Area            = "indexes"
	AreaPartitionedTables           Area            = "partitioned_tables"
	AreaDeclarativeModel            Area            = "declarative_model"
	AreaTypesAndNonTableObjects     Area            = "types_and_non_table_objects"
	AreaDataAndWholeTableOperations Area            = "data_and_whole_table_operations"
	TierOne                         Tier            = "t1"
	TierTwo                         Tier            = "t2"
	TierThree                       Tier            = "t3"
	StatusSupported                 StatusMark      = "✅"
	StatusPlanned                   StatusMark      = "🟡"
	StatusNoSafetyProblem           StatusMark      = "⚪"
	StatusOtherTool                 StatusMark      = "🔵"
	StatusNoOnlineMechanism         StatusMark      = "❌"
	PathNativeAsIs                  EnginePath      = "native_as_is"
	PathNativeSaferSequence         EnginePath      = "native_safer_sequence"
	PathNativePlannedFlow           EnginePath      = "native_planned_flow"
	PathCopyAndSwap                 EnginePath      = "copy_and_swap"
	PathNone                        EnginePath      = "none"
	DoorSupported                   FrontDoorStatus = "supported"
	DoorRefused                     FrontDoorStatus = "refused"
	DoorNotApplicable               FrontDoorStatus = "not_applicable"
)

// FrontDoors records admission through the imperative and declarative interfaces.
type FrontDoors struct {
	Migrate FrontDoorStatus `yaml:"migrate" json:"migrate"`
	Diff    FrontDoorStatus `yaml:"diff" json:"diff"`
}

// Row is one machine-readable support-matrix entry.
type Row struct {
	ID                  string         `yaml:"id" json:"id"`
	Area                Area           `yaml:"area" json:"area"`
	Operation           string         `yaml:"operation" json:"operation"`
	Tier                Tier           `yaml:"tier" json:"tier"`
	StatusMark          StatusMark     `yaml:"status_mark" json:"status_mark"`
	EnginePath          EnginePath     `yaml:"engine_path" json:"engine_path"`
	OnlineSafetyProblem bool           `yaml:"online_safety_problem" json:"online_safety_problem"`
	OnlineSafetyDetail  string         `yaml:"online_safety_detail,omitempty" json:"online_safety_detail,omitempty"`
	OwningToolClass     string         `yaml:"owning_tool_class,omitempty" json:"owning_tool_class,omitempty"`
	FrontDoors          FrontDoors     `yaml:"front_doors" json:"front_doors"`
	RefusalReason       verdict.Reason `yaml:"refusal_reason,omitempty" json:"refusal_reason,omitempty"`
	ReasonNotes         string         `yaml:"reason_notes" json:"reason_notes"`
}

type document struct {
	Rows []Row `yaml:"rows"`
}

// Load parses and validates capability YAML.
func Load(data []byte) ([]Row, error) {
	var doc document
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&doc); err != nil {
		return nil, fmt.Errorf("parse capabilities YAML: %w", err)
	}
	if err := Validate(doc.Rows); err != nil {
		return nil, err
	}
	return doc.Rows, nil
}

// Rows returns the validated embedded capability rows in display order.
func Rows() ([]Row, error) { return Load(source) }

var idPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// Validate checks closed vocabularies and cross-field invariants.
func Validate(rows []Row) error {
	ids := map[string]bool{}
	for i, row := range rows {
		prefix := fmt.Sprintf("row %d", i+1)
		if !idPattern.MatchString(row.ID) {
			return fmt.Errorf("%s: id %q is not lowercase kebab-case", prefix, row.ID)
		}
		if ids[row.ID] {
			return fmt.Errorf("%s: duplicate id %q", prefix, row.ID)
		}
		ids[row.ID] = true
		prefix = fmt.Sprintf("row %d (%s)", i+1, row.ID)
		if err := validateEnums(row); err != nil {
			return fmt.Errorf("%s: %w", prefix, err)
		}
		if err := validateText(row); err != nil {
			return fmt.Errorf("%s: %w", prefix, err)
		}
		expected := map[StatusMark]Tier{StatusSupported: TierOne, StatusPlanned: TierTwo, StatusNoSafetyProblem: TierThree, StatusOtherTool: TierThree, StatusNoOnlineMechanism: TierThree}[row.StatusMark]
		if row.Tier != expected {
			return fmt.Errorf("%s: status_mark does not agree with tier", prefix)
		}
		if row.Tier == TierThree && row.EnginePath != PathNone {
			return fmt.Errorf("%s: T3 requires engine_path none", prefix)
		}
		if row.EnginePath == PathNone && row.Tier != TierThree {
			return fmt.Errorf("%s: engine_path none requires T3", prefix)
		}
		if !row.OnlineSafetyProblem && row.OwningToolClass == "" {
			return fmt.Errorf("%s: No row requires owning_tool_class", prefix)
		}
		if row.OnlineSafetyProblem && row.OwningToolClass != "" {
			return fmt.Errorf("%s: Yes row cannot name owning_tool_class", prefix)
		}
		refused := row.FrontDoors.Migrate == DoorRefused || row.FrontDoors.Diff == DoorRefused
		if refused != (row.RefusalReason != "") {
			return fmt.Errorf("%s: refusal_reason must be present iff a front door is refused", prefix)
		}
		if row.RefusalReason != "" && !slices.Contains(verdict.Reasons(), row.RefusalReason) {
			return fmt.Errorf("%s: refusal_reason %q is not a verdict.Reason", prefix, row.RefusalReason)
		}
	}
	return nil
}

// validateEnums checks every closed-vocabulary field of one row and names
// the first field and value that fall outside their vocabulary.
func validateEnums(row Row) error {
	if err := checkEnum("area", row.Area, AreaColumnChanges, AreaConstraints, AreaIndexes, AreaPartitionedTables, AreaDeclarativeModel, AreaTypesAndNonTableObjects, AreaDataAndWholeTableOperations); err != nil {
		return err
	}
	if err := checkEnum("tier", row.Tier, TierOne, TierTwo, TierThree); err != nil {
		return err
	}
	if err := checkEnum("status_mark", row.StatusMark, StatusSupported, StatusPlanned, StatusNoSafetyProblem, StatusOtherTool, StatusNoOnlineMechanism); err != nil {
		return err
	}
	if err := checkEnum("engine_path", row.EnginePath, PathNativeAsIs, PathNativeSaferSequence, PathNativePlannedFlow, PathCopyAndSwap, PathNone); err != nil {
		return err
	}
	if err := checkEnum("front_doors.migrate", row.FrontDoors.Migrate, DoorSupported, DoorRefused, DoorNotApplicable); err != nil {
		return err
	}
	return checkEnum("front_doors.diff", row.FrontDoors.Diff, DoorSupported, DoorRefused, DoorNotApplicable)
}

func checkEnum[T ~string](field string, value T, allowed ...T) error {
	if slices.Contains(allowed, value) {
		return nil
	}
	return fmt.Errorf("%s %q is not one of %v", field, string(value), allowed)
}

// validateText checks the free-text cells: the required ones are present,
// and none carries a newline. Every cell is interpolated into one line of a
// pipe-delimited Markdown table, and a newline would end the row early and
// hide every row after it while the generator still reports success.
func validateText(row Row) error {
	if row.Operation == "" {
		return errors.New("operation is required")
	}
	if row.ReasonNotes == "" {
		return errors.New("reason_notes is required")
	}
	for field, value := range textCells(row) {
		if strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("%s must be a single line (a table row cannot span lines)", field)
		}
	}
	return nil
}

// textCells names every free-text field that lands in a rendered table cell.
func textCells(row Row) map[string]string {
	return map[string]string{
		"operation":            row.Operation,
		"online_safety_detail": row.OnlineSafetyDetail,
		"owning_tool_class":    row.OwningToolClass,
		"reason_notes":         row.ReasonNotes,
	}
}

// cell escapes a free-text value for one Markdown table cell: an unescaped
// pipe would split the cell and shift every column after it.
func cell(value string) string {
	return strings.ReplaceAll(value, "|", "\\|")
}

// RenderDocument replaces every marked generated region in a capabilities document.
func RenderDocument(input []byte, rows []Row) ([]byte, error) {
	areaHeadings := map[Area]string{AreaColumnChanges: "Operation", AreaConstraints: "Operation", AreaIndexes: "Operation", AreaPartitionedTables: "Operation", AreaDeclarativeModel: "Table shape", AreaTypesAndNonTableObjects: "Object / operation", AreaDataAndWholeTableOperations: "Operation"}
	pathLabels := map[EnginePath]string{PathNativeAsIs: "native, as-is", PathNativeSaferSequence: "native, safer sequence", PathNativePlannedFlow: "native, planned flow", PathCopyAndSwap: "copy-and-swap", PathNone: "—"}
	out := append([]byte(nil), input...)
	counts := map[StatusMark]int{}
	tiers := map[Tier]int{}
	for _, r := range rows {
		counts[r.StatusMark]++
		tiers[r.Tier]++
	}
	summary := fmt.Sprintf("**%d operations: %d supported today, %d planned behind a typed refusal, %d out of scope\nby design, and %d with no online mechanism in PostgreSQL to build on.**", len(rows), tiers[TierOne], tiers[TierTwo], counts[StatusNoSafetyProblem]+counts[StatusOtherTool], counts[StatusNoOnlineMechanism])
	var err error
	out, err = replaceRegion(out, "summary", summary)
	if err != nil {
		return nil, err
	}
	for _, area := range []Area{AreaColumnChanges, AreaConstraints, AreaIndexes, AreaPartitionedTables, AreaDeclarativeModel, AreaTypesAndNonTableObjects, AreaDataAndWholeTableOperations} {
		var b strings.Builder
		fmt.Fprintf(&b, "| %s | Status | Engine path | Online-safety problem? | Behavior and why |\n| --- | --- | --- | --- | --- |\n", areaHeadings[area])
		for _, r := range rows {
			if r.Area != area {
				continue
			}
			answer := "Yes"
			if !r.OnlineSafetyProblem {
				answer = "No — " + r.OwningToolClass
			} else if r.OnlineSafetyDetail != "" {
				answer += " — " + r.OnlineSafetyDetail
			}
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n", cell(r.Operation), r.StatusMark, pathLabels[r.EnginePath], cell(answer), cell(r.ReasonNotes))
		}
		out, err = replaceRegion(out, string(area), strings.TrimSuffix(b.String(), "\n"))
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// replaceRegion replaces the bytes between one region's begin and end
// markers. The document must carry exactly one of each, begin first; any
// other arrangement is refused before either slice so a malformed document
// is never truncated.
func replaceRegion(doc []byte, name, content string) ([]byte, error) {
	begin := []byte("<!-- capabilities:begin " + name + " -->")
	end := []byte("<!-- capabilities:end " + name + " -->")
	bi, err := markerIndex(doc, begin)
	if err != nil {
		return nil, err
	}
	ei, err := markerIndex(doc, end)
	if err != nil {
		return nil, err
	}
	if ei < bi {
		return nil, fmt.Errorf("capability marker %q appears before %q", end, begin)
	}
	start := bi + len(begin)
	replacement := []byte("\n" + content + "\n")
	return append(append(append([]byte(nil), doc[:start]...), replacement...), doc[ei:]...), nil
}

// markerIndex locates one marker that must appear exactly once.
func markerIndex(doc, marker []byte) (int, error) {
	switch bytes.Count(doc, marker) {
	case 0:
		return 0, fmt.Errorf("capability marker %q is missing", marker)
	case 1:
		return bytes.Index(doc, marker), nil
	default:
		return 0, fmt.Errorf("capability marker %q appears more than once", marker)
	}
}
