// Package capabilities provides the embedded, validated support matrix and its Markdown renderer.
package capabilities

import (
	"bytes"
	_ "embed"
	"fmt"
	"regexp"
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
	areas := set(AreaColumnChanges, AreaConstraints, AreaIndexes, AreaPartitionedTables, AreaDeclarativeModel, AreaTypesAndNonTableObjects, AreaDataAndWholeTableOperations)
	tiers := set(TierOne, TierTwo, TierThree)
	marks := set(StatusSupported, StatusPlanned, StatusNoSafetyProblem, StatusOtherTool, StatusNoOnlineMechanism)
	paths := set(PathNativeAsIs, PathNativeSaferSequence, PathNativePlannedFlow, PathCopyAndSwap, PathNone)
	doors := set(DoorSupported, DoorRefused, DoorNotApplicable)
	reasons := map[verdict.Reason]bool{}
	for _, reason := range verdict.Reasons() {
		reasons[reason] = true
	}
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
		if !areas[row.Area] || !tiers[row.Tier] || !marks[row.StatusMark] || !paths[row.EnginePath] || !doors[row.FrontDoors.Migrate] || !doors[row.FrontDoors.Diff] {
			return fmt.Errorf("%s: unknown enum value", prefix)
		}
		if row.Operation == "" || row.ReasonNotes == "" {
			return fmt.Errorf("%s: operation and reason_notes are required", prefix)
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
		if row.RefusalReason != "" && !reasons[row.RefusalReason] {
			return fmt.Errorf("%s: unknown refusal_reason %q", prefix, row.RefusalReason)
		}
	}
	return nil
}

func set[T comparable](values ...T) map[T]bool {
	out := make(map[T]bool, len(values))
	for _, v := range values {
		out[v] = true
	}
	return out
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
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n", r.Operation, r.StatusMark, pathLabels[r.EnginePath], answer, r.ReasonNotes)
		}
		out, err = replaceRegion(out, string(area), strings.TrimSuffix(b.String(), "\n"))
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func replaceRegion(doc []byte, name, content string) ([]byte, error) {
	begin := []byte("<!-- capabilities:begin " + name + " -->")
	end := []byte("<!-- capabilities:end " + name + " -->")
	bi := bytes.Index(doc, begin)
	ei := bytes.Index(doc, end)
	if bi < 0 || ei < 0 || ei < bi || bytes.Contains(doc[bi+len(begin):], begin) || bytes.Contains(doc[ei+len(end):], end) {
		return nil, fmt.Errorf("missing, unbalanced, or duplicate capability markers for %s", name)
	}
	start := bi + len(begin)
	replacement := []byte("\n" + content + "\n")
	return append(append(append([]byte(nil), doc[:start]...), replacement...), doc[ei:]...), nil
}
