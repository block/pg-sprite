package hosted_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Diagnose publication startup without applying DDL or retrying a failed assertion.
func TestHostedRealtimeContinuousBaseline(t *testing.T) {
	f := newFixture(t)
	f.seed(t)
	s := f.subscribe(t, 0)
	done := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for i := 10; i < 30; i++ {
			select {
			case <-t.Context().Done():
				done <- t.Context().Err()
				return
			case <-ticker.C:
			}
			_, err := f.pool.Exec(t.Context(), "INSERT INTO "+f.table+" VALUES ($1,$2,'baseline')", i, f.users[0].ID)
			if err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	t.Cleanup(func() { require.NoError(t, <-done) })
	seen := make(map[int]bool)
	s.await(t, func(e event) bool {
		if e.Event == "postgres_changes" {
			seen[e.Payload.Data.Record.ID] = true
			t.Logf("received %d of 20 distinct baseline writes", len(seen))
		}
		return len(seen) == 20
	})
}
