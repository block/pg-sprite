package migrate

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// The restart acknowledgement is a typed table name that must match the
// statement's resolved target exactly, like the force acknowledgement: a
// flag left on a script from another table's run must not discard this
// one's.
func TestCheckRestartAck(t *testing.T) {
	st, err := statement.ParseOne("ALTER TABLE app.orders DROP COLUMN note")
	require.NoError(t, err)

	assert.NoError(t, checkRestartAck(st, "app.orders"))
	for name, ack := range map[string]string{
		"another table":     "app.customers",
		"unqualified":       "orders",
		"different casing":  "app.Orders",
		"wrong schema":      "public.orders",
		"empty after strip": "",
	} {
		t.Run("rejects "+name, func(t *testing.T) {
			err := checkRestartAck(st, ack)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "nothing was executed")
		})
	}
}

// Starting over is a copy-and-swap notion: a statement the router executes
// through the copy-and-swap backend is the only one with a shadow, a
// hold, and a row to discard. An acknowledgement on a statement routed
// anywhere else is an error, not a no-op, because the operator who typed
// it expected a leftover to go.
func TestCheckRestartRoute(t *testing.T) {
	st, err := statement.ParseOne("ALTER TABLE app.orders DROP COLUMN note")
	require.NoError(t, err)

	assert.NoError(t, checkRestartRoute(st, router.Statement{Disposition: router.DispositionExecute, Backend: router.BackendCopyAndSwap}))
	for name, rs := range map[string]router.Statement{
		"native execute":           {Disposition: router.DispositionExecute, Backend: router.BackendNative},
		"copy-and-swap but refuse": {Disposition: router.DispositionRefuse, Backend: router.BackendCopyAndSwap},
		"no route at all":          {},
	} {
		t.Run("rejects "+name, func(t *testing.T) {
			err := checkRestartRoute(st, rs)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "nothing was executed")
		})
	}
}

// The restart acknowledges a copy-and-swap run; Force and AcceptBlocking
// each route the statement away from that backend, so either together
// with a restart is rejected at the front door before any database work.
func TestRunRejectsRestartWithAnotherAcknowledgement(t *testing.T) {
	st, err := statement.ParseOne("ALTER TABLE app.orders DROP COLUMN note")
	require.NoError(t, err)

	for name, with := range map[string]func(*Options){
		"force":           func(o *Options) { o.Force = "app.orders" },
		"accept blocking": func(o *Options) { o.AcceptBlocking = "app.orders" },
	} {
		t.Run(name, func(t *testing.T) {
			opts := DefaultOptions()
			opts.CopySwap.Restart = "app.orders"
			with(&opts)

			v, err := Run(t.Context(), nil, st, opts)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "CopySwap.Restart")
			assert.Equal(t, verdict.Verdict{}, v)
		})
	}
}

// Desired-state execution never starts a stopped run over: the derived
// plan, not the operator, would decide which run the acknowledgement
// discards, so it is rejected as a typed sentinel before any database
// work, even when it names a table the plan would touch.
func TestRunDesiredRejectsRestart(t *testing.T) {
	opts := DefaultOptions()
	opts.CopySwap.Restart = "app.orders"
	res, err := RunDesired(t.Context(), nil, DesiredRequest{Schema: "app"}, opts)
	require.ErrorIs(t, err, ErrRestartNotSupported)
	assert.Equal(t, DesiredResult{}, res)
}

// An embedder branching on the sentinels sees the acknowledgement the
// desired-state door rejects even when the Options value also breaks
// validate's rule that Restart excludes Force and AcceptBlocking. Force is
// checked first, so Restart beside Force reads as ErrForceNotSupported;
// AcceptBlocking is not rejected by this door, so Restart beside it reads
// as ErrRestartNotSupported.
func TestRunDesiredNamesTheRejectedAcknowledgementBesideAnother(t *testing.T) {
	opts := DefaultOptions()
	opts.CopySwap.Restart = "app.orders"
	opts.Force = "app.orders"
	_, err := RunDesired(t.Context(), nil, DesiredRequest{Schema: "app"}, opts)
	require.ErrorIs(t, err, ErrForceNotSupported)

	opts = DefaultOptions()
	opts.CopySwap.Restart = "app.orders"
	opts.AcceptBlocking = "app.orders"
	_, err = RunDesired(t.Context(), nil, DesiredRequest{Schema: "app"}, opts)
	require.ErrorIs(t, err, ErrRestartNotSupported)
}
