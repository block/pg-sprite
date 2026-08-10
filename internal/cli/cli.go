// Package cli defines the pg-sprite command tree (Kong). migrate and status
// are implemented (the Phase 1 optimistic front door); the remaining
// subcommand Run methods are stubs each build-plan phase fills in.
package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
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
	Status  StatusCmd  `cmd:"" help:"Report the status of a running schema change."`
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
	Debug            bool          `help:"Log statement-level tracing and lifecycle diagnostics to stderr."`

	// diagOut overrides the diagnostics destination (stderr) in tests. Kong
	// ignores unexported fields.
	diagOut io.Writer
}

// Config translates the flags into the connectivity layer's configuration.
// The tracer is wired only under --debug: dbconn skips statement tracing
// entirely for a nil logger.
func (f DBFlags) Config() dbconn.Config {
	cfg := dbconn.Config{
		URL:              f.URL,
		CACertPath:       f.CACert,
		LockTimeout:      f.LockTimeout,
		StatementTimeout: f.StatementTimeout,
	}
	if f.Debug {
		cfg.Logger = f.diag()
	}
	return cfg
}

// diag returns the diagnostics logger: debug-level text on stderr (or the
// test override) under --debug, a discarding logger otherwise. Diagnostics
// never share stdout with command output.
func (f DBFlags) diag() *slog.Logger {
	if !f.Debug {
		return slog.New(slog.DiscardHandler)
	}
	out := f.diagOut
	if out == nil {
		out = os.Stderr
	}
	return slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// MigrateCmd runs a schema change (imperative front-end): the Phase 1
// optimistic front door. Easy changes execute directly under tight budgets;
// everything else is refused with an explicit verdict.
type MigrateCmd struct {
	DBFlags `embed:""`

	Alter        string   `help:"Imperative ALTER statement to run." name:"alter" required:""`
	MaxTableSize byteSize `help:"Size threshold above which the optimistic attempt is skipped, measured as the table's full on-disk footprint: heap, indexes, and TOAST, all partitions (binary units: B, KiB, MiB, GiB, TiB)." default:"1GiB"`
	DryRun       bool     `help:"Classify and route the statement, print the plan, and execute nothing."`
	JSON         bool     `help:"Emit the verdict (or dry-run plan) as JSON."`
}

// Run implements the migrate subcommand.
func (c *MigrateCmd) Run() error { return c.run(context.Background(), os.Stdout) }

// DiffCmd derives statements from a desired-state schema (declarative
// front-end): introspect the live table, materialize the desired state on a
// rolled-back scratch schema, and print the ordered plan without executing
// anything.
type DiffCmd struct {
	DBFlags `embed:""`

	Desired string `help:"Path to the desired-state CREATE TABLE .sql file." name:"desired" type:"existingfile" required:""`
	Schema  string `help:"Schema containing the live table." default:"public"`
	JSON    bool   `help:"Emit the plan as JSON."`
}

// Run implements the diff subcommand.
func (c *DiffCmd) Run() error { return c.run(context.Background(), os.Stdout) }

// FmtCmd canonicalizes a schema file. It is offline — no database flags.
type FmtCmd struct {
	Path string `arg:"" optional:"" help:"Schema file to format; stdin when omitted." type:"existingfile"`
}

// Run implements the fmt subcommand.
func (c *FmtCmd) Run() error { return c.runFmt(os.Stdin, os.Stdout) }

// LintCmd checks DDL for unsafe or unsupported patterns.
type LintCmd struct{}

// Run implements the lint subcommand.
func (c *LintCmd) Run() error { return notImplemented("lint") }

// StatusCmd reports schema-change progress.
type StatusCmd struct {
	DBFlags `embed:""`

	JSON bool `help:"Emit the session listing as JSON."`
}

// Run implements the status subcommand.
func (c *StatusCmd) Run() error { return c.run(context.Background(), os.Stdout) }
