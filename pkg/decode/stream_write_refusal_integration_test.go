package decode_test

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/block/pg-sprite/pkg/decode"
)

// errWriteRefused is what a refusing connection's writes fail with once
// the test has asked it to refuse them.
var errWriteRefused = errors.New("write refused by the test")

// refusingWrites is a connection whose writes fail once refuse is set
// while its reads go on, so a report can fail on a connection the wait
// before it ran out on: closing the connection instead fails the read
// first and never reaches the report.
type refusingWrites struct {
	net.Conn
	refuse *atomic.Bool
}

func (c *refusingWrites) Write(b []byte) (int, error) {
	if c.refuse.Load() {
		return 0, errWriteRefused
	}
	return c.Conn.Write(b)
}

// openStreamRefusingWrites opens the route's stream on a replication
// connection whose writes fail once refuse is set. The connection is
// dialed through the config's before-connect hook, the same way the
// engine's own credential hook reaches the dial.
func (f slotFixture) openStreamRefusingWrites(t *testing.T, from decode.LSN, refuse *atomic.Bool) fixtureStream {
	t.Helper()
	cfg := f.cfg
	dialer := &net.Dialer{}
	cfg.BeforeConnect = func(_ context.Context, cc *pgx.ConnConfig) error {
		cc.DialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return &refusingWrites{Conn: c, refuse: refuse}, nil
		}
		return nil
	}
	return f.openStreamWith(t, cfg, from)
}
