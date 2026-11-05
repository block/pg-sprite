package migrate

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/preflight"
)

// The evidence's instants are UTC whatever zone pgx scanned them in, so the
// verdict's JSON does not depend on the host's time zone, and a partitioned
// index carries its leaf count while a plain one carries none.
func TestIndexUsageEvidenceIsUTCAndCountsPartitions(t *testing.T) {
	east := time.FixedZone("UTC+8", 8*60*60)
	readAt := time.Date(2026, 11, 2, 11, 13, 17, 0, east)
	reset := readAt.Add(-48 * time.Hour)
	last := readAt.Add(-time.Hour)

	e := indexUsageEvidence(preflight.IndexUsage{
		IdxScans: 5, ReadAt: readAt, StatsReset: &reset, LastIdxScan: &last,
		Partitioned: true, Partitions: 2, TrackCounts: true, TrackCountsOffBySetting: true,
	})

	assert.Equal(t, time.UTC, e.ReadAt.Location())
	assert.True(t, e.ReadAt.Equal(readAt))
	require.NotNil(t, e.StatsReset)
	assert.Equal(t, time.UTC, e.StatsReset.Location())
	require.NotNil(t, e.LastIdxScan)
	assert.Equal(t, time.UTC, e.LastIdxScan.Location())
	require.NotNil(t, e.Partitions)
	assert.Equal(t, 2, *e.Partitions)
	assert.EqualValues(t, 5, e.IdxScans)
	assert.True(t, e.TrackCountsOffBySetting)

	plain := indexUsageEvidence(preflight.IndexUsage{ReadAt: readAt, TrackCounts: true})
	assert.Nil(t, plain.Partitions)
	assert.Nil(t, plain.StatsReset)
	assert.Nil(t, plain.LastIdxScan)
}

// describeIndexUsage is the detail's renderer: each piece of evidence and
// each caveat appears exactly when its field says so.
func TestDescribeIndexUsage(t *testing.T) {
	reset := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	last := time.Date(2026, 11, 1, 8, 30, 0, 0, time.UTC)
	const created = " or since the index was created, if later (the server does not record when)"

	cases := []struct {
		name  string
		usage preflight.IndexUsage
		want  string
	}{
		{
			name:  "no reset recorded",
			usage: preflight.IndexUsage{TrackCounts: true},
			want:  "this server has scanned the index 0 times since its statistics began (no reset recorded)" + created,
		},
		{
			name:  "reset and last scan",
			usage: preflight.IndexUsage{IdxScans: 7, StatsReset: &reset, LastIdxScan: &last, TrackCounts: true},
			want: "this server has scanned the index 7 times since stats_reset at 2026-09-01T00:00:00Z" + created +
				", last at 2026-11-01T08:30:00Z",
		},
		{
			name:  "partitioned and unique",
			usage: preflight.IndexUsage{IdxScans: 5, Partitioned: true, Partitions: 2, Unique: true, TrackCounts: true},
			want: "this server has scanned the index through its partition indexes (2) 5 times since its statistics began (no reset recorded)" +
				created + "; it is a unique index",
		},
		{
			name:  "track_counts off",
			usage: preflight.IndexUsage{},
			want: "this server has scanned the index 0 times since its statistics began (no reset recorded)" + created +
				"; track_counts is off, so this server counted no scans and the count proves nothing",
		},
		{
			name:  "track_counts off by a saved setting",
			usage: preflight.IndexUsage{TrackCounts: true, TrackCountsOffBySetting: true},
			want: "this server has scanned the index 0 times since its statistics began (no reset recorded)" + created +
				"; an ALTER ROLE or ALTER DATABASE setting turns track_counts off, so scans by those sessions are not counted",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, describeIndexUsage(tc.usage))
		})
	}
}
