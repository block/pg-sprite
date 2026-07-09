// Package cli defines the pg-sprite command tree (Kong). Subcommand Run
// methods are stubs; each build-plan phase fills one in.
package cli

import (
	"fmt"
	"time"

	"github.com/alecthomas/kong"

	"github.com/block/pg-sprite/pkg/dbconn"
)

// CLI is the root command tree.
type CLI struct {
	Version kong.VersionFlag `help:"Print version and exit."`

	Migrate MigrateCmd `cmd:"" help:"Run a schema change safely."`
	Diff    DiffCmd    `cmd:"" help:"Diff a desired-state schema file against the live schema."`
	Fmt     FmtCmd     `cmd:"" help:"Canonicalize a schema file."`
	Lint    LintCmd    `cmd:"" help:"Lint DDL for unsafe patterns."`
	Status  StatusCmd  `cmd:"" help:"Report the status of a running migration."`
}

// New returns an empty command tree for kong.Parse.
func New() *CLI { return &CLI{} }

func notImplemented(cmd string) error {
	return fmt.Errorf("%s: not implemented yet (Phase 0 stub)", cmd)
}

// DBFlags are the connection flags shared by every command that talks to the
// database, so every entry point carries the same bounded session defaults.
type DBFlags struct {
	URL              string        `help:"PostgreSQL connection URL or key=value DSN." env:"PGSPRITE_URL" required:""`
	CACert           string        `help:"CA bundle path for verify-full TLS. RDS/Aurora endpoints verify with the embedded bundle automatically." env:"PGSPRITE_CA_CERT" type:"existingfile"`
	LockTimeout      time.Duration `help:"Session lock_timeout applied to every statement." default:"3s"`
	StatementTimeout time.Duration `help:"Session statement_timeout applied to every statement." default:"30s"`
}

// Config translates the flags into the connectivity layer's configuration.
func (f DBFlags) Config() dbconn.Config {
	return dbconn.Config{
		URL:              f.URL,
		CACertPath:       f.CACert,
		LockTimeout:      f.LockTimeout,
		StatementTimeout: f.StatementTimeout,
	}
}

// MigrateCmd runs a schema change (imperative front-end).
type MigrateCmd struct {
	DBFlags `embed:""`

	Alter string `help:"Imperative ALTER statement to run." name:"alter"`
}

// Run implements the migrate subcommand.
func (c *MigrateCmd) Run() error { return notImplemented("migrate") }

// DiffCmd derives statements from a desired-state schema (declarative front-end).
type DiffCmd struct {
	DBFlags `embed:""`

	Desired string `help:"Path to the desired-state CREATE TABLE .sql file." name:"desired" type:"existingfile" required:""`
}

// Run implements the diff subcommand.
func (c *DiffCmd) Run() error { return notImplemented("diff") }

// FmtCmd canonicalizes a schema file. It is offline — no database flags.
type FmtCmd struct {
	Path string `arg:"" optional:"" help:"Schema file to format." type:"existingfile"`
}

// Run implements the fmt subcommand.
func (c *FmtCmd) Run() error { return notImplemented("fmt") }

// LintCmd checks DDL for unsafe or unsupported patterns.
type LintCmd struct{}

// Run implements the lint subcommand.
func (c *LintCmd) Run() error { return notImplemented("lint") }

// StatusCmd reports migration progress.
type StatusCmd struct {
	DBFlags `embed:""`
}

// Run implements the status subcommand.
func (c *StatusCmd) Run() error { return notImplemented("status") }
