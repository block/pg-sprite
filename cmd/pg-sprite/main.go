// Command pg-sprite is an online schema-change engine for Aurora PostgreSQL.
package main

import (
	"github.com/alecthomas/kong"

	"github.com/block/pg-sprite/internal/cli"
)

// version is stamped at release time via -ldflags "-X main.version=…".
var version = "dev"

func main() {
	k := kong.Parse(cli.New(),
		kong.Name("pg-sprite"),
		kong.Description("An online schema-change engine for Aurora PostgreSQL."),
		kong.UsageOnError(),
		kong.Vars{"version": version},
	)
	k.FatalIfErrorf(k.Run())
}
