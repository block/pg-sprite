package change

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func watchKey(id int64) RowWatch {
	return RowWatch{Schema: "app", Table: "ledger", Match: func(key []any) bool { return key[0] == id }}
}

// One verification runs at a time.
func TestParkerRefusesASecondWatch(t *testing.T) {
	var r rowParker
	_, err := r.arm(watchKey(5))
	require.NoError(t, err)

	_, err = r.arm(watchKey(6))
	require.ErrorIs(t, err, errVerificationRunning)
}

// A change to the watched row fires the watch with that change and removes
// it; a change to another row or another table does not.
func TestParkerFiresOnlyOnTheWatchedRow(t *testing.T) {
	var r rowParker
	p, err := r.arm(watchKey(5))
	require.NoError(t, err)

	assert.Nil(t, r.take(ledgerTable(), rowChange{key: []any{int64(6)}}))
	other := &TableInfo{SchemaName: "app", TableName: "other", Columns: []string{"id"}, KeyColumns: []string{"id"}}
	assert.Nil(t, r.take(other, rowChange{key: []any{int64(5)}}))

	change := rowChange{key: []any{int64(5)}, image: []any{"5", "edited"}}
	assert.Same(t, p, r.take(ledgerTable(), change))
	assert.Equal(t, change, p.change)
	select {
	case <-p.fired:
	default:
		t.Fatal("the watch did not fire")
	}
	assert.Nil(t, r.take(ledgerTable(), change), "a fired watch is removed")
}

// A held reader goes on once the verifier releases it, and a disarmed
// watch no longer fires.
func TestParkerReleasesTheReader(t *testing.T) {
	var r rowParker
	p, err := r.arm(watchKey(5))
	require.NoError(t, err)

	var reader sync.WaitGroup
	reader.Go(func() { r.offer(t.Context(), ledgerTable(), rowChange{key: []any{int64(5)}}) })
	<-p.fired
	p.unpark()
	reader.Wait()

	q, err := r.arm(watchKey(5))
	require.NoError(t, err)
	r.disarm(q)
	assert.Nil(t, r.take(ledgerTable(), rowChange{key: []any{int64(5)}}))
}
