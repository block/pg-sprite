package executor

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The proof's name is derived from its kind and expression alone, so a
// rerun finds the CHECK an earlier run added, and another bound, or the
// other kind, never takes it for its own. It fits the identifier limit, so
// the server never truncates it into a name the read would miss.
func TestAttachProofNameIsDerivedFromKindAndExpression(t *testing.T) {
	const january = `"day" IS NOT NULL AND "day" OPERATOR("pg_catalog".>=) CAST('2027-01-01' AS date) AND "day" OPERATOR("pg_catalog".<) CAST('2027-02-01' AS date)`
	const february = `"day" IS NOT NULL AND "day" OPERATOR("pg_catalog".>=) CAST('2027-02-01' AS date) AND "day" OPERATOR("pg_catalog".<) CAST('2027-03-01' AS date)`
	bound := attachProof{kind: attachProofBound, tableOID: 1, schema: "app", table: "events_2027_01", expr: january}

	again := bound
	again.tableOID, again.table = 2, "events_renamed"
	assert.Equal(t, bound.name(), again.name(), "the table does not enter the name")

	other := bound
	other.expr = february
	assert.NotEqual(t, bound.name(), other.name(), "another bound gets another name")

	def := bound
	def.kind = attachProofDefault
	assert.NotEqual(t, bound.name(), def.name(), "the other kind gets another name")

	assert.True(t, strings.HasPrefix(bound.name(), "pgsprite_attach_bound_"), bound.name())
	assert.True(t, strings.HasPrefix(def.name(), "pgsprite_attach_default_"), def.name())
	assert.LessOrEqual(t, len(def.name()), maxIdentifierBytes)
}

// Only a CHECK under the name is the proof: a validated one is done, a NOT
// VALID one waits for its validation, and any other constraint holding the
// name is refused.
func TestDecideAttachProof(t *testing.T) {
	p := attachProof{kind: attachProofBound, tableOID: 1, schema: "app", table: "events_2027_01", expr: "true"}

	state, oid, err := decideAttachProof(p, "c", "true", true, 42)
	require.NoError(t, err)
	assert.Equal(t, attachProofDone, state)
	assert.Equal(t, uint32(42), oid)

	state, _, err = decideAttachProof(p, "c", "true", false, 42)
	require.NoError(t, err)
	assert.Equal(t, attachProofValidatePending, state)

	for _, contype := range []string{"u", "p", "f", "x"} {
		_, _, err := decideAttachProof(p, contype, "", true, 42)
		assert.ErrorIs(t, err, errAttachProofNameTaken, contype)
	}
}

// A parent's CHECK keeps the parent's name, which the attach matches by, and
// is the proof only when it also carries the parent's expression as the
// server prints it: the same name over another expression is refused.
func TestDecideAttachProofForAParentsCheck(t *testing.T) {
	p := attachProof{kind: attachProofParent, tableOID: 1, schema: "app", table: "payments_2027",
		expr: "(amount > 0)", constraint: "payments_amount_positive"}
	assert.Equal(t, "payments_amount_positive", p.name())

	state, _, err := decideAttachProof(p, "c", "(amount > 0)", true, 7)
	require.NoError(t, err)
	assert.Equal(t, attachProofDone, state)

	_, _, err = decideAttachProof(p, "c", "(amount >= 0)", true, 7)
	assert.ErrorIs(t, err, errAttachProofNameTaken)
}

// A proof with no kind, no resolved table, or no expression is a caller's
// mistake, refused as an invariant violation before anything is read.
func TestAttachProofValidateRefusesAnIncompleteProof(t *testing.T) {
	good := attachProof{kind: attachProofDefault, tableOID: 1, schema: "app", table: "events_default", expr: "true"}
	require.NoError(t, good.validate())
	for name, mutate := range map[string]func(*attachProof){
		"unknown kind":                          func(p *attachProof) { p.kind = "other" },
		"unresolved table":                      func(p *attachProof) { p.tableOID = 0 },
		"no schema":                             func(p *attachProof) { p.schema = "" },
		"no expression":                         func(p *attachProof) { p.expr = "" },
		"a derived kind with a constraint name": func(p *attachProof) { p.constraint = "named" },
		"a parent's CHECK with no name":         func(p *attachProof) { p.kind = attachProofParent },
	} {
		p := good
		mutate(&p)
		assert.ErrorIs(t, p.validate(), ErrInvariantViolation, name)
	}
}
