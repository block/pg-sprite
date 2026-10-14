package checkpoint

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The proof is NULL when the run has none, and otherwise the JSON it holds,
// whether the caller handed in a nil or an empty slice for none.
func TestShadowProofColumn(t *testing.T) {
	assert.Nil(t, shadowProofColumn(nil))
	assert.Nil(t, shadowProofColumn(json.RawMessage{}))
	assert.Equal(t, json.RawMessage(`{"a":1}`), shadowProofColumn(json.RawMessage(`{"a":1}`)))
}

// Two renderings of one document are the same proof whatever their spacing
// and key order — which is how the server hands a jsonb value back — while
// a changed value, a missing key, a reordered array, or a number written
// another way is a different one; a side that is not JSON is an error, not
// a difference.
func TestSameJSONComparesByValue(t *testing.T) {
	saved := json.RawMessage(`{"shadow_table": "_pgsprite_orders_new", "indexes": [{"name": "a"}, {"name": "b"}], "rows": 12345678901234567890}`)
	rendered := json.RawMessage(`{"rows": 12345678901234567890, "indexes": [{"name": "a"}, {"name": "b"}], "shadow_table": "_pgsprite_orders_new"}`)
	same, err := sameJSON(saved, rendered)
	require.NoError(t, err)
	assert.True(t, same, "spacing and key order do not make a different proof")

	different := map[string]json.RawMessage{
		"changed value":    json.RawMessage(`{"shadow_table": "_pgsprite_orders_new2", "indexes": [{"name": "a"}, {"name": "b"}], "rows": 12345678901234567890}`),
		"missing key":      json.RawMessage(`{"shadow_table": "_pgsprite_orders_new", "indexes": [{"name": "a"}, {"name": "b"}]}`),
		"extra key":        json.RawMessage(`{"shadow_table": "_pgsprite_orders_new", "indexes": [{"name": "a"}, {"name": "b"}], "rows": 12345678901234567890, "x": 1}`),
		"reordered array":  json.RawMessage(`{"shadow_table": "_pgsprite_orders_new", "indexes": [{"name": "b"}, {"name": "a"}], "rows": 12345678901234567890}`),
		"number as text":   json.RawMessage(`{"shadow_table": "_pgsprite_orders_new", "indexes": [{"name": "a"}, {"name": "b"}], "rows": "12345678901234567890"}`),
		"array for object": json.RawMessage(`[]`),
	}
	for name, other := range different {
		t.Run(name, func(t *testing.T) {
			same, err := sameJSON(saved, other)
			require.NoError(t, err)
			assert.False(t, same)
		})
	}

	_, err = sameJSON(saved, json.RawMessage(`{"shadow_table": `))
	assert.Error(t, err)
}

// A Save replaces the row's proof only when both sides carry one and they
// differ; a side without a proof replaces nothing, and the same proof in
// the server's rendering is not a replacement.
func TestReplacesProofOnlyWhenBothSidesDiffer(t *testing.T) {
	stored := row{shadowProof: []byte(`{"indexes": [{"name": "a"}], "shadow_table": "_pgsprite_orders_new"}`)}
	cp := Checkpoint{ShadowProof: json.RawMessage(`{"shadow_table":"_pgsprite_orders_new","indexes":[{"name":"a"}]}`)}

	replaces, err := replacesProof(stored, cp)
	require.NoError(t, err)
	assert.False(t, replaces, "the same proof in another rendering is not a replacement")

	replaces, err = replacesProof(row{}, cp)
	require.NoError(t, err)
	assert.False(t, replaces, "a row without a proof has nothing to replace")

	replaces, err = replacesProof(stored, Checkpoint{})
	require.NoError(t, err)
	assert.False(t, replaces, "a Save without a proof keeps the row's")

	cp.ShadowProof = json.RawMessage(`{"shadow_table":"_pgsprite_orders_new2","indexes":[{"name":"a"}]}`)
	replaces, err = replacesProof(stored, cp)
	require.NoError(t, err)
	assert.True(t, replaces)

	_, err = replacesProof(row{shadowProof: []byte(`not json`)}, cp)
	assert.ErrorIs(t, err, ErrInvariantViolation, "a row the column cannot hold is a state no Save wrote")
}
