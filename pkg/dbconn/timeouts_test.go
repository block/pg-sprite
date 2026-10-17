package dbconn

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// PostgreSQL reads a bare integer timeout as milliseconds and reads 0 as "no
// timeout", so a positive duration below one millisecond would truncate to 0
// and send the session out unbounded. The refusal names the field and the
// value so the caller can see which budget to raise.
func TestBuildPoolConfigRefusesATimeoutBelowOneMillisecond(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		message string
	}{
		{"lock 500us", Config{URL: testURL, LockTimeout: 500 * time.Microsecond}, "LockTimeout 500µs"},
		{"statement 999us", Config{URL: testURL, StatementTimeout: 999 * time.Microsecond}, "StatementTimeout 999µs"},
		{"lock 1ns", Config{URL: testURL, LockTimeout: time.Nanosecond}, "LockTimeout 1ns"},
		{"lock negative", Config{URL: testURL, LockTimeout: -time.Second}, "LockTimeout -1s"},
		{"statement negative", Config{URL: testURL, StatementTimeout: -time.Millisecond}, "StatementTimeout -1ms"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pc, err := buildPoolConfig(tt.cfg)

			require.ErrorIs(t, err, ErrInvalidTimeout)
			assert.Contains(t, err.Error(), tt.message)
			assert.Nil(t, pc, "no pool configuration is produced for a refused timeout")
		})
	}
}

// One millisecond is the smallest timeout PostgreSQL can hold; a duration that
// is not a whole number of milliseconds truncates toward zero but never to it,
// so it stays bounded. Zero keeps meaning "use the default".
func TestBuildPoolConfigAcceptsTimeoutsOfAtLeastOneMillisecond(t *testing.T) {
	tests := []struct {
		name      string
		lock      time.Duration
		statement time.Duration
		wantLock  string
		wantStmt  string
	}{
		{"exactly 1ms", time.Millisecond, time.Millisecond, "1", "1"},
		{"1500us truncates to 1ms, not to 0", 1500 * time.Microsecond, 2500 * time.Microsecond, "1", "2"},
		{"zero keeps the defaults", 0, 0, "3000", "30000"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pc, err := buildPoolConfig(Config{URL: testURL, LockTimeout: tt.lock, StatementTimeout: tt.statement})
			require.NoError(t, err)

			assert.Equal(t, tt.wantLock, pc.ConnConfig.RuntimeParams["lock_timeout"])
			assert.Equal(t, tt.wantStmt, pc.ConnConfig.RuntimeParams["statement_timeout"])
		})
	}
}
