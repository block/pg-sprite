package dbconn

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// afterConnectHook is work the pool runs on every new physical connection,
// before the connection is handed to a caller.
type afterConnectHook func(ctx context.Context, conn *pgx.Conn) error

// chainAfterConnect runs hooks in order on each new connection, stopping at
// the first error so a connection is never handed out half-prepared.
//
// pgx offers a single AfterConnect slot and more than one thing has to
// happen on a new connection, so the slot holds a chain rather than whichever
// hook was assigned last. Assigning the field directly would make each new
// hook silently replace the previous one — a change that compiles, passes a
// non-nil assertion on the field, and removes a guarantee.
func chainAfterConnect(hooks ...afterConnectHook) afterConnectHook {
	return func(ctx context.Context, conn *pgx.Conn) error {
		for _, hook := range hooks {
			if err := hook(ctx, conn); err != nil {
				return err
			}
		}
		return nil
	}
}
