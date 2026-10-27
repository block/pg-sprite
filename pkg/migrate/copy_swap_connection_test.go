package migrate

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The mismatch error names both sides so an embedder's log says which
// database each handle reached, and it is recovered through a wrap with
// errors.As. The two shapes an operator can hit read differently: a
// connection on another cluster names both clusters, since two clusters
// can each hold a database of one name; a connection on the pool's cluster
// but another database says so without the cluster noise.
func TestCopySwapConnectionMismatchError(t *testing.T) {
	t.Run("another cluster names both clusters", func(t *testing.T) {
		err := &CopySwapConnectionMismatchError{
			PoolCluster: "7001", PoolDatabase: "billing",
			ConnectionCluster: "7002", ConnectionDatabase: "billing",
		}
		msg := err.Error()
		assert.Contains(t, msg, `database "billing" on cluster 7002`)
		assert.Contains(t, msg, `database "billing" on cluster 7001`)
	})

	t.Run("another database on the same cluster names the databases", func(t *testing.T) {
		err := &CopySwapConnectionMismatchError{
			PoolCluster: "7001", PoolDatabase: "billing",
			ConnectionCluster: "7001", ConnectionDatabase: "billing_staging",
		}
		msg := err.Error()
		assert.Contains(t, msg, `reaches database "billing_staging", the pool database "billing" on the same cluster`)
		assert.NotContains(t, msg, "7001")
	})

	t.Run("is recovered through a wrap", func(t *testing.T) {
		wrapped := fmt.Errorf("migrate: %w", &CopySwapConnectionMismatchError{
			PoolCluster: "7001", PoolDatabase: "billing",
			ConnectionCluster: "7001", ConnectionDatabase: "billing_staging",
		})
		var mismatch *CopySwapConnectionMismatchError
		require.ErrorAs(t, wrapped, &mismatch)
		assert.Equal(t, "billing_staging", mismatch.ConnectionDatabase)
		assert.False(t, errors.Is(wrapped, ErrCopySwapConnectionRequired), "a wired connection is not the unset one")
	})
}
