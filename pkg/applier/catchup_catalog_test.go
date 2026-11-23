package applier

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
)

// The catch-up reads the server's write position and snapshot on the
// caller's pool every cycle, so the reads cannot rely on the session's
// search_path: every function and cast they spell is pg_catalog-qualified
// (CO-9). This pins the SQL text itself; the integration test proves the
// behaviour under a lookalike text type.
func TestCatchupPoolReadsQualifyEveryFunctionAndCast(t *testing.T) {
	for name, query := range map[string]string{
		"walEndSQL":   walEndSQL,
		"snapshotSQL": snapshotSQL,
	} {
		t.Run(name, func(t *testing.T) {
			testutil.AssertCatalogQualified(t, query)
		})
	}
}

// ReadSnapshot sends the server exactly snapshotSQL, so the checked
// constant is the statement a caller's pool receives, not a copy beside it.
func TestReadSnapshotRunsTheQualifiedStatement(t *testing.T) {
	db := &recordingQuerier{answer: "100:105:102"}

	snap, err := ReadSnapshot(t.Context(), db)

	require.NoError(t, err)
	assert.Equal(t, []string{snapshotSQL}, db.statements)
	assert.Equal(t, Snapshot{read: true, xmin: 100, xmax: 105, inProgress: map[uint64]struct{}{102: {}}}, snap)
}

// recordingQuerier records each statement it is asked to run and answers
// every one with a single text column.
type recordingQuerier struct {
	answer     string
	statements []string
}

func (q *recordingQuerier) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	q.statements = append(q.statements, sql)
	return textRow(q.answer)
}

type textRow string

func (r textRow) Scan(dest ...any) error {
	*dest[0].(*string) = string(r)
	return nil
}
