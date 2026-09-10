package capabilities

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func validRow() Row {
	return Row{ID: "example", Area: AreaColumnChanges, Operation: "example", Tier: TierOne, StatusMark: StatusSupported, EnginePath: PathNativeAsIs, OnlineSafetyProblem: true, FrontDoors: FrontDoors{Migrate: DoorSupported, Diff: DoorSupported}, ReasonNotes: "notes"}
}

func TestEmbeddedCapabilitiesValidate(t *testing.T) {
	rows, err := Rows()
	require.NoError(t, err)
	assert.Len(t, rows, 53)
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

func TestCheckedInMarkdownIsGenerated(t *testing.T) {
	rows, err := Rows()
	require.NoError(t, err)
	input, err := os.ReadFile("../../docs/capabilities.md")
	require.NoError(t, err)
	output, err := RenderDocument(input, rows)
	require.NoError(t, err)
	assert.Equal(t, input, output)
}

func TestRenderDocumentRejectsBadMarkers(t *testing.T) {
	rows, err := Rows()
	require.NoError(t, err)
	for _, doc := range []string{"", "<!-- capabilities:begin summary -->"} {
		_, err := RenderDocument([]byte(doc), rows)
		assert.Error(t, err)
	}
}
