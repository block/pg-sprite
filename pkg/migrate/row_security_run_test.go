package migrate

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

func TestRowSecurityVerdictPreservesUnknownCommit(t *testing.T) {
	err := &executor.RowSecurityOutcomeUnknownError{Err: context.Canceled}
	v := rowSecurityVerdict("public.documents", executor.RowSecurityReport{}, err)
	assert.Equal(t, verdict.OutcomeFailed, v.Outcome)
	assert.Equal(t, string(executor.CodeRowSecurityOutcomeUnknown), v.Code)
	assert.Empty(t, v.ExecutedSQL)
	assert.Equal(t, err.Error(), v.Detail)
}

func TestRowSecurityVerdictPreservesCancellation(t *testing.T) {
	v := rowSecurityVerdict("public.documents", executor.RowSecurityReport{}, context.Canceled)
	assert.Equal(t, verdict.OutcomeFailed, v.Outcome)
	assert.Equal(t, string(executor.OutcomeCode(context.Canceled)), v.Code)
	assert.Empty(t, v.ExecutedSQL)
}

// The row security change runs in one transaction under its own lock: it
// has no plan to pin and acknowledges nothing, so a pin, a destructive
// acknowledgement, or an acknowledgement in Options is rejected before any
// database work — the nil pool is never reached — and so is a run with no
// connection to take the table's lock from.
func TestRunRowSecurityRejectsWhatItCannotHonourBeforeAnyDatabaseWork(t *testing.T) {
	desired, err := statement.ParseDesiredWithRowSecurity(`
		CREATE TABLE documents (
			id int PRIMARY KEY
		);
		ALTER TABLE documents ENABLE ROW LEVEL SECURITY;`)
	require.NoError(t, err)
	restart := DefaultOptions()
	restart.CopySwap.Restart = "public.documents"
	for name, tc := range map[string]struct {
		req  RowSecurityRequest
		opts Options
		want error
	}{
		"pin": {
			req:  RowSecurityRequest{Schema: "public", Desired: desired, ExpectedFingerprint: "abc"},
			opts: DefaultOptions(), want: errAtomicRowSecurityPin,
		},
		"destructive acknowledgement": {
			req:  RowSecurityRequest{Schema: "public", Desired: desired, AllowDestructive: "public.documents"},
			opts: DefaultOptions(), want: errAtomicRowSecurityDestructive,
		},
		"restart acknowledgement in Options": {
			req:  RowSecurityRequest{Schema: "public", Desired: desired},
			opts: restart, want: ErrRestartNotSupported,
		},
		// The table's lock is taken on a session of its own, so the
		// connection it is opened from must be wired.
		"no Options.Connection": {
			req:  RowSecurityRequest{Schema: "public", Desired: desired},
			opts: DefaultOptions(), want: ErrConnectionRequired,
		},
	} {
		t.Run(name, func(t *testing.T) {
			v, err := RunRowSecurity(t.Context(), nil, tc.req, tc.opts)
			require.ErrorIs(t, err, tc.want)
			assert.Equal(t, verdict.Outcome(""), v.Outcome, "nothing reached the table")
			v, err = RunReviewedRowSecurity(t.Context(), nil, tc.req, nil, tc.opts)
			require.ErrorIs(t, err, tc.want)
			assert.Equal(t, verdict.Outcome(""), v.Outcome, "nothing reached the table")
		})
	}
}
