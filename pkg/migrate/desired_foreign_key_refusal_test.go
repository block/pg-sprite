package migrate

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/verdict"
)

// A refused foreign-key drop carries the plan statement's cause on the
// verdict, so a host routes a key changed under the same name apart from a
// key a desired file cannot declare. A statement with no cause refuses with
// the reason alone.
func TestStatementRefusalCarriesTheForeignKeyCause(t *testing.T) {
	for _, cause := range []verdict.Cause{verdict.CauseChangedForeignKey, verdict.CauseUndeclarableForeignKey, ""} {
		r, _ := statementRefusal(0, plan.Statement{
			SQL:          `ALTER TABLE app.events DROP CONSTRAINT events_user_fk`,
			Disposition:  router.DispositionRefuse,
			Reason:       verdict.ReasonDeclarativeForeignKey,
			Class:        verdict.ClassCapabilityBoundary,
			RefusalCause: cause,
		})
		assert.Equal(t, verdict.ReasonDeclarativeForeignKey, r.Reason(), cause)
		assert.Equal(t, cause, r.Cause(), cause)
	}
}

// The detail of a refused foreign-key drop names the one remedy that
// applies: a key changed under the same name is declared under a new name,
// and a key a desired file cannot declare is removed through the imperative
// front door. Without a cause, it names both.
func TestForeignKeyDropRefusalDetailNamesTheRemedy(t *testing.T) {
	changed := foreignKeyDropRefusalDetail(verdict.CauseChangedForeignKey)
	assert.Contains(t, changed, "declare the changed key under a new name")
	assert.NotContains(t, changed, "migrate --alter")

	undeclarable := foreignKeyDropRefusalDetail(verdict.CauseUndeclarableForeignKey)
	assert.Contains(t, undeclarable, "migrate --alter")
	assert.NotContains(t, undeclarable, "under a new name")

	either := foreignKeyDropRefusalDetail("")
	assert.Contains(t, either, "under a new name")
	assert.Contains(t, either, "migrate --alter")
}
