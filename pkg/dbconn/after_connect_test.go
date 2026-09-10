package dbconn

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A chained hook runs every link, in the order given. The pool's single
// AfterConnect slot carries more than one preparation step, and asserting
// only that the field is non-nil cannot tell a chain from one step that
// replaced another.
func TestChainAfterConnectRunsEveryHookInOrder(t *testing.T) {
	var ran []string
	chain := chainAfterConnect(
		func(context.Context, *pgx.Conn) error { ran = append(ran, "first"); return nil },
		func(context.Context, *pgx.Conn) error { ran = append(ran, "second"); return nil },
		func(context.Context, *pgx.Conn) error { ran = append(ran, "third"); return nil },
	)

	require.NoError(t, chain(t.Context(), nil))

	assert.Equal(t, []string{"first", "second", "third"}, ran)
}

// A failing hook stops the chain, so a connection is never handed to a
// caller with only part of its preparation applied.
func TestChainAfterConnectStopsAtTheFirstFailure(t *testing.T) {
	boom := errors.New("boom")
	var ran []string
	chain := chainAfterConnect(
		func(context.Context, *pgx.Conn) error { ran = append(ran, "first"); return nil },
		func(context.Context, *pgx.Conn) error { ran = append(ran, "second"); return boom },
		func(context.Context, *pgx.Conn) error { ran = append(ran, "third"); return nil },
	)

	err := chain(t.Context(), nil)

	require.ErrorIs(t, err, boom)
	assert.Equal(t, []string{"first", "second"}, ran,
		"the hook after the failure must not run")
}
