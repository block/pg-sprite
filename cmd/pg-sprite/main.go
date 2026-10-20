// Command pg-sprite is an online schema-change engine for PostgreSQL.
package main

import (
	"context"
	"errors"
	"os"

	"github.com/alecthomas/kong"

	"github.com/block/pg-sprite/internal/cli"
	"github.com/block/pg-sprite/pkg/verdict"
)

// version is stamped at release time via -ldflags "-X main.version=…".
var version = "dev"

func main() {
	k, err := run()
	// A refusal verdict was already printed; its exit code is distinct from
	// operational errors so automation can branch on the difference.
	if errors.Is(err, verdict.ErrRefused) {
		os.Exit(verdict.ExitCodeRefused)
	}
	if errors.Is(err, verdict.ErrAcceptedBlocking) {
		os.Exit(verdict.ExitCodeAcceptedBlocking)
	}
	k.FatalIfErrorf(err)
}

// run parses and runs the command under a context the operator's SIGINT
// or SIGTERM cancels: the first signal lets the command stop on its own
// terms (release locks, checkpoint, print its verdict), the second ends
// the process. Offline commands take no context and are unaffected. The
// signal handler is released when the command returns, before main maps
// the error to an exit code.
func run() (*kong.Context, error) {
	ctx, stop := cli.Interruptible(context.Background())
	defer stop()
	k := kong.Parse(cli.New(version),
		kong.Name("pg-sprite"),
		kong.Description("An online schema-change engine for PostgreSQL."),
		kong.UsageOnError(),
		kong.Vars{"version": version},
		kong.BindTo(ctx, (*context.Context)(nil)),
	)
	return k, k.Run()
}
