package statement

import (
	"testing"

	pganalyze "github.com/pganalyze/pg_query_go/v6"
	"github.com/stretchr/testify/assert"
)

// A namespaced option cannot be held by a StorageParam, so it is reported
// unrenderable rather than rendered under its bare name, which would set a
// different option. A constraint's grammar never produces one; the check
// keeps the reader fail-closed for any element it is handed.
func TestStorageParamRefusesANamespacedOption(t *testing.T) {
	seventy := &pganalyze.Node{Node: &pganalyze.Node_Integer{Integer: &pganalyze.Integer{Ival: 70}}}

	_, ok := storageParam(&pganalyze.DefElem{Defnamespace: "toast", Defname: "fillfactor", Arg: seventy})
	assert.False(t, ok)

	param, ok := storageParam(&pganalyze.DefElem{Defname: "fillfactor", Arg: seventy})
	assert.True(t, ok)
	assert.Equal(t, StorageParam{Name: "fillfactor", Value: "70", HasValue: true}, param)
}
