package testutil

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func validSpec() LoadSpec {
	return LoadSpec{Seed: 1, Workers: 2, RatePerSecond: 10, Mix: Mix{Insert: 1, Update: 1}, HotRowFraction: 0.2, ToastRewriteFraction: 0.5}
}

func TestLoadSpecValidate(t *testing.T) {
	require.NoError(t, validSpec().validate())
	cases := map[string]struct {
		mutate func(*LoadSpec)
		want   string
	}{
		"zero workers":       {func(s *LoadSpec) { s.Workers = 0 }, "load spec: workers must be at least 1, got 0"},
		"negative rate":      {func(s *LoadSpec) { s.RatePerSecond = -5 }, "load spec: rate per second must be at least 1, got -5"},
		"zero rate":          {func(s *LoadSpec) { s.RatePerSecond = 0 }, "load spec: rate per second must be at least 1, got 0"},
		"negative weight":    {func(s *LoadSpec) { s.Mix.Delete = -1 }, "load spec: mix weights must not be negative, got {Insert:1 Update:1 Delete:-1 UniqueMove:0}"},
		"empty mix":          {func(s *LoadSpec) { s.Mix = Mix{} }, "load spec: mix must have positive total weight, got {Insert:0 Update:0 Delete:0 UniqueMove:0}"},
		"hot fraction > 1":   {func(s *LoadSpec) { s.HotRowFraction = 1.5 }, "load spec: hot row fraction must be within [0, 1], got 1.5"},
		"toast fraction < 0": {func(s *LoadSpec) { s.ToastRewriteFraction = -0.1 }, "load spec: toast rewrite fraction must be within [0, 1], got -0.1"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			spec := validSpec()
			tc.mutate(&spec)
			assert.EqualError(t, spec.validate(), tc.want)
		})
	}
}

// A generator whose workers never finish still hands back what it counted
// and the worker error that explains the wedge, joined with the deadline.
func TestStopReturnsSummaryAndWorkerErrorOnDeadline(t *testing.T) {
	_, cancel := context.WithCancel(t.Context())
	wedged := errors.New("pool closed under worker")
	g := &LoadGenerator{cancel: cancel, done: make(chan struct{}), stopTimeout: 10 * time.Millisecond}
	g.summary = Summary{Inserts: 3, Races: 1, InsertedIDs: []int64{7, 8, 9}}
	g.fail(wedged)

	summary, err := g.Stop()

	assert.Equal(t, Summary{Inserts: 3, Races: 1, InsertedIDs: []int64{7, 8, 9}}, summary)
	require.ErrorIs(t, err, wedged)
	assert.ErrorContains(t, err, "stop load generator: workers still running after 10ms")
}

// Stop on a generator that finished cleanly reports its summary with no error.
func TestStopAfterCleanFinish(t *testing.T) {
	_, cancel := context.WithCancel(t.Context())
	g := &LoadGenerator{cancel: cancel, done: make(chan struct{}), stopTimeout: stopTimeout}
	g.summary = Summary{Updates: 2}
	close(g.done)

	summary, err := g.Stop()

	require.NoError(t, err)
	assert.Equal(t, Summary{Updates: 2}, summary)
}
