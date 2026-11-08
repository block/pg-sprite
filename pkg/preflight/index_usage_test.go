package preflight

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// A leaf index the statistics view does not list has unknown scans, so the
// read fails closed rather than counting it as zero. A live server lists
// every index of a table, which is why this is checked here and not through
// ReadIndexUsage.
func TestCheckLeafStatisticsListedRefusesAnUnlistedLeaf(t *testing.T) {
	assert.ErrorIs(t, checkLeafStatisticsListed(2, 1), errIndexUsageStatisticsMissing)
	assert.ErrorIs(t, checkLeafStatisticsListed(1, 0), errIndexUsageStatisticsMissing)
}

// Every leaf listed, including a partitioned index with no partitions (zero
// leaves, zero rows), is a complete read.
func TestCheckLeafStatisticsListedAcceptsEveryLeafListed(t *testing.T) {
	assert.NoError(t, checkLeafStatisticsListed(2, 2))
	assert.NoError(t, checkLeafStatisticsListed(0, 0))
}
