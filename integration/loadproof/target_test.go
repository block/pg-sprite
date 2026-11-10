package loadproof_test

import (
	"net"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// targetIsLocal reports whether every host the connection settings name is
// on this machine: a Unix-domain socket directory, localhost, or a loopback
// address. The proof seeds and copies a large table on its target, so a
// target elsewhere is run against only when the operator says it is
// disposable.
func targetIsLocal(cfg *pgconn.Config) bool {
	if !hostIsLocal(cfg.Host) {
		return false
	}
	for _, fallback := range cfg.Fallbacks {
		if !hostIsLocal(fallback.Host) {
			return false
		}
	}
	return true
}

// hostIsLocal reports whether one host is on this machine.
func hostIsLocal(host string) bool {
	if isUnixSocketDir(host) {
		return true
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// isUnixSocketDir reports whether host names a Unix-domain socket
// directory, which libpq-style settings spell as an absolute path.
func isUnixSocketDir(host string) bool {
	return len(host) > 0 && host[0] == '/'
}

func dsnIsLocal(t *testing.T, dsn string) bool {
	t.Helper()
	cfg, err := pgconn.ParseConfig(dsn)
	require.NoError(t, err)
	return targetIsLocal(cfg)
}

// The compose database, a loopback address of either family, and a Unix
// socket are this machine's: the proof runs against them without asking.
func TestTargetIsLocalAcceptsThisMachine(t *testing.T) {
	assert.True(t, dsnIsLocal(t, "postgres://u:p@localhost:5432/db"))
	assert.True(t, dsnIsLocal(t, "postgres://u:p@127.0.0.1:5432/db"))
	assert.True(t, dsnIsLocal(t, "postgres://u:p@[::1]:5432/db"))
	assert.True(t, dsnIsLocal(t, "host=/tmp dbname=db user=u"))
}

// A remote host is refused, and so is a host list that falls back to one:
// the fallback is where the proof's table lands when the first host is
// down.
func TestTargetIsLocalRefusesARemoteHost(t *testing.T) {
	assert.False(t, dsnIsLocal(t, "postgres://u:p@db.example.com:5432/db"))
	assert.False(t, dsnIsLocal(t, "postgres://u:p@10.0.0.5:5432/db"))
	assert.False(t, dsnIsLocal(t, "postgres://u:p@localhost:5432,db.example.com:5432/db"))
}
