// Command pg-sprite is an online schema-change engine for PostgreSQL.
package main

import (
	"errors"
	"os"

	"github.com/alecthomas/kong"

	"github.com/block/pg-sprite/internal/cli"
	"github.com/block/pg-sprite/pkg/verdict"
)

// version is stamped at release time via -ldflags "-X main.version=…".
var version = "dev"

func main() {
	k := kong.Parse(cli.New(),
		kong.Name("pg-sprite"),
		kong.Description("An online schema-change engine for PostgreSQL."),
		kong.UsageOnError(),
		kong.Vars{"version": version},
	)
	err := k.Run()
	// A refusal verdict was already printed; its exit code is distinct from
	// operational errors so automation can branch on the difference.
	if errors.Is(err, verdict.ErrRefused) {
		os.Exit(verdict.ExitCodeRefused)
	}
	k.FatalIfErrorf(err)
}
