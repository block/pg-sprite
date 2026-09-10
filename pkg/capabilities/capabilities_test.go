package capabilities

import (
	"encoding/json"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/verdict"
)

func validRow() Row {
	return Row{ID: "example", Area: AreaColumnChanges, Operation: "example", Tier: TierOne, StatusMark: StatusSupported, EnginePath: PathNativeAsIs, OnlineSafetyProblem: true, FrontDoors: FrontDoors{Migrate: DoorSupported, Diff: DoorSupported}, ReasonNotes: "notes"}
}

func TestEmbeddedCapabilitiesValidate(t *testing.T) {
	rows, err := Rows()
	require.NoError(t, err)
	assert.Len(t, rows, 53)
}

// The engine refuses a partitioned parent with its own target-fact reason
// (partitionedParentVerdict in pkg/migrate, one row per PartitionRefusalCause
// family), so the rows for those shapes must publish that token, not the
// planner-level unsupported-statement: a consumer filtering on the reason
// the engine emits would otherwise find no rows.
func TestPartitionedParentRowsCarryTheEngineReason(t *testing.T) {
	rows, err := Rows()
	require.NoError(t, err)
	var got []string
	for _, r := range rows {
		if r.RefusalReason == verdict.ReasonUnsupportedPartitionedParent {
			got = append(got, r.ID)
		}
	}
	assert.ElementsMatch(t, []string{
		"index-build-on-a-partitioned-parent",
		"add-constraint-using-index-on-a-partitioned-parent",
	}, got)
	for _, r := range rows {
		if strings.Contains(r.Operation, "partitioned parent") && r.RefusalReason != "" {
			assert.Equal(t, verdict.ReasonUnsupportedPartitionedParent, r.RefusalReason,
				"row %s names a partitioned parent but publishes %q", r.ID, r.RefusalReason)
		}
	}
}

func TestLoadRejectsInvalidYAML(t *testing.T) {
	_, err := Load([]byte("rows: ["))
	assert.ErrorContains(t, err, "parse capabilities YAML")
}

func TestValidateRules(t *testing.T) {
	tests := map[string]func(*Row){
		"unknown area":              func(r *Row) { r.Area = "unknown" },
		"unknown tier":              func(r *Row) { r.Tier = "unknown" },
		"unknown status mark":       func(r *Row) { r.StatusMark = "unknown" },
		"unknown engine path":       func(r *Row) { r.EnginePath = "unknown" },
		"unknown front door status": func(r *Row) { r.FrontDoors.Migrate = "unknown" },
		"duplicate id":              func(r *Row) {},
		"invalid id":                func(r *Row) { r.ID = "Not valid" },
		"required text":             func(r *Row) { r.Operation = "" },
		"mark agrees with tier":     func(r *Row) { r.Tier = TierTwo },
		"T3 path is none": func(r *Row) {
			r.Tier = TierThree
			r.StatusMark = StatusOtherTool
			r.EnginePath = PathNativeAsIs
			r.OnlineSafetyProblem = false
			r.OwningToolClass = "owner"
		},
		"none only on T3":            func(r *Row) { r.EnginePath = PathNone },
		"No names owner":             func(r *Row) { r.OnlineSafetyProblem = false },
		"Yes omits owner":            func(r *Row) { r.OwningToolClass = "owner" },
		"refusal reason iff refused": func(r *Row) { r.FrontDoors.Diff = DoorRefused },
		"known refusal reason":       func(r *Row) { r.FrontDoors.Diff = DoorRefused; r.RefusalReason = "unknown" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			row := validRow()
			mutate(&row)
			rows := []Row{row}
			if name == "duplicate id" {
				rows = append(rows, row)
			}
			assert.Error(t, Validate(rows))
		})
	}
}

// An enum error names the row, the field, and the offending value, so a
// typo in a 53-row file is found without diffing the vocabulary by hand.
func TestValidateNamesTheFieldAndValue(t *testing.T) {
	tests := map[string]struct {
		mutate func(*Row)
		want   string
	}{
		"area":         {func(r *Row) { r.Area = "colum_changes" }, `area "colum_changes" is not one of`},
		"engine path":  {func(r *Row) { r.EnginePath = "native" }, `engine_path "native" is not one of`},
		"diff door":    {func(r *Row) { r.FrontDoors.Diff = "refuse" }, `front_doors.diff "refuse" is not one of`},
		"migrate door": {func(r *Row) { r.FrontDoors.Migrate = "ok" }, `front_doors.migrate "ok" is not one of`},
		"refusal reason": {func(r *Row) {
			r.FrontDoors.Diff = DoorRefused
			r.RefusalReason = "unsupported-partition"
		}, `refusal_reason "unsupported-partition" is not a verdict.Reason`},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			row := validRow()
			tt.mutate(&row)
			err := Validate([]Row{row})
			require.Error(t, err)
			assert.ErrorContains(t, err, "row 1 (example): ")
			assert.ErrorContains(t, err, tt.want)
		})
	}
}

// A free-text cell that spans lines would end its table row early and
// silently drop every row after it, so the loader refuses it.
func TestValidateRejectsMultilineCells(t *testing.T) {
	for field, mutate := range map[string]func(*Row){
		"operation":            func(r *Row) { r.Operation = "one\ntwo" },
		"reason_notes":         func(r *Row) { r.ReasonNotes = "one\ntwo" },
		"online_safety_detail": func(r *Row) { r.OnlineSafetyDetail = "one\r\ntwo" },
		"owning_tool_class": func(r *Row) {
			r.OnlineSafetyProblem = false
			r.OwningToolClass = "one\ntwo"
		},
	} {
		t.Run(field, func(t *testing.T) {
			row := validRow()
			mutate(&row)
			assert.ErrorContains(t, Validate([]Row{row}), field+" must be a single line")
		})
	}
}

// A YAML block scalar is the realistic way a newline reaches a cell.
func TestLoadRejectsBlockScalarCell(t *testing.T) {
	_, err := Load([]byte(`rows:
  - id: example
    area: column_changes
    operation: example
    tier: t1
    status_mark: "✅"
    engine_path: native_as_is
    online_safety_problem: true
    front_doors:
      migrate: supported
      diff: supported
    reason_notes: |
      first line
      second line
`))
	assert.ErrorContains(t, err, "reason_notes must be a single line")
}

func TestCheckedInMarkdownIsGenerated(t *testing.T) {
	rows, err := Rows()
	require.NoError(t, err)
	input, err := os.ReadFile("../../docs/capabilities.md")
	require.NoError(t, err)
	output, err := RenderDocument(input, rows)
	require.NoError(t, err)
	assert.Equal(t, input, output)
}

// The contract doc promises that one named row marshals to the JSON object
// it prints. Consumers copy that object's field values into jq filters, so
// the example must be the row's actual encoding, not a paraphrase of it.
func TestContractDocExampleRowMatchesRegistry(t *testing.T) {
	raw, err := os.ReadFile("../../docs/capabilities-contract.md")
	require.NoError(t, err)
	blocks := regexp.MustCompile("(?s)```json\n(\\{\n  \"id\": \"[^\"]+\",.*?)```").FindAllStringSubmatch(string(raw), -1)
	require.Len(t, blocks, 1, "the contract doc prints one example row object")

	var example struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal([]byte(blocks[0][1]), &example))

	rows, err := Rows()
	require.NoError(t, err)
	index := slices.IndexFunc(rows, func(row Row) bool { return row.ID == example.ID })
	require.GreaterOrEqual(t, index, 0, "example row %q is not in the registry", example.ID)
	marshaled, err := json.Marshal(rows[index])
	require.NoError(t, err)
	assert.JSONEq(t, string(marshaled), blocks[0][1],
		"docs/capabilities-contract.md example row drifted from capabilities.yaml")
}

// A pipe inside a cell is escaped so GFM keeps it in that cell; every
// other column stays in place and the escaped source renders as the pipe.
func TestRenderDocumentEscapesPipesInCells(t *testing.T) {
	row := validRow()
	row.Operation = "`a | b`"
	row.OnlineSafetyDetail = "either|or"
	row.ReasonNotes = "left | right"
	var doc strings.Builder
	doc.WriteString("<!-- capabilities:begin summary -->\n<!-- capabilities:end summary -->\n")
	for _, area := range []Area{AreaColumnChanges, AreaConstraints, AreaIndexes, AreaPartitionedTables, AreaDeclarativeModel, AreaTypesAndNonTableObjects, AreaDataAndWholeTableOperations} {
		doc.WriteString("<!-- capabilities:begin " + string(area) + " -->\n<!-- capabilities:end " + string(area) + " -->\n")
	}
	out, err := RenderDocument([]byte(doc.String()), []Row{row})
	require.NoError(t, err)
	want := "| `a \\| b` | ✅ | native, as-is | Yes — either\\|or | left \\| right |\n"
	assert.Contains(t, string(out), want)
	for line := range strings.SplitSeq(string(out), "\n") {
		if strings.HasPrefix(line, "| `a") {
			assert.Equal(t, 5, markdownCellCount(line), "the row must still have five cells: %s", line)
		}
	}
}

// markdownCellCount counts the cells a Markdown table renderer would see:
// unescaped pipes split cells, escaped ones (\|) are literal text.
func markdownCellCount(line string) int {
	unescaped := strings.ReplaceAll(line, "\\|", "")
	return strings.Count(unescaped, "|") - 1
}

// Each malformed marker arrangement is refused with an error that names the
// marker and the problem, and nothing is written for any of them.
func TestRenderDocumentRejectsBadMarkers(t *testing.T) {
	rows, err := Rows()
	require.NoError(t, err)
	const begin, end = "<!-- capabilities:begin summary -->", "<!-- capabilities:end summary -->"
	tests := map[string]struct{ doc, want string }{
		"empty":            {"", `begin summary -->" is missing`},
		"missing end":      {begin, `end summary -->" is missing`},
		"end before begin": {end + "\n" + begin, `end summary -->" appears before`},
		"duplicate begin":  {begin + "\n" + begin + "\n" + end, `begin summary -->" appears more than once`},
		"duplicate end":    {begin + "\n" + end + "\n" + end, `end summary -->" appears more than once`},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			out, err := RenderDocument([]byte(tt.doc), rows)
			assert.Nil(t, out)
			assert.ErrorContains(t, err, tt.want)
		})
	}
}
