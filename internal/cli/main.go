package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/alecthomas/kong"

	"github.com/block/pg-sprite/pkg/verdict"
)

// exitFailed is the process exit code for an operational or usage error: a
// command line that does not parse, or a command that fails before or
// without producing a verdict. Exit codes 2 and 3 are the verdict package's.
const exitFailed = 1

// kongExit carries the exit code kong requests after --help or --version
// from the kong.Exit hook back to Main, which recovers it.
type kongExit struct{ code int }

// Main parses args, runs the selected command, and returns the process exit
// code; it never exits the process itself. stdout is the destination for
// --help and --version output only, and both return 0; commands write their
// results to the process stdout themselves. A command line that does not
// parse is reported on stderr — never stdout, which under --json carries
// only the machine-readable product — and exits exitFailed.
func Main(args []string, version string, stdout, stderr io.Writer) (code int) {
	// Kong ends --help and --version by calling its Exit hook from inside
	// Parse. The hook panics with kongExit to unwind out of Parse, and this
	// recover turns it into Main's return value; any other panic propagates.
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		exit, ok := r.(kongExit)
		if !ok {
			panic(r)
		}
		code = exit.code
	}()

	parser, err := kong.New(New(version),
		kong.Name("pg-sprite"),
		kong.Description("An online schema-change engine for PostgreSQL."),
		kong.Writers(stdout, stderr),
		kong.Exit(func(code int) { panic(kongExit{code: code}) }),
		kong.Vars{"version": version},
	)
	if err != nil {
		// Nowhere left to report a failed stderr write.
		_, _ = fmt.Fprintf(stderr, "pg-sprite: error: build command tree: %v\n", err)
		return exitFailed
	}

	kctx, err := parser.Parse(args)
	if err != nil {
		return reportParseError(parser, err)
	}

	err = runInterruptible(kctx)
	// A refusal verdict was already printed; its exit code is distinct from
	// operational errors so automation can branch on the difference.
	//
	// The exit mapping is closed and public: 0 success, 1 failed, 2 refused,
	// 3 accepted-blocking. A new code is added as a pkg/verdict sentinel with
	// its own case here, not through an ExitCode() method on an error.
	switch {
	case errors.Is(err, verdict.ErrRefused):
		return verdict.ExitCodeRefused
	case errors.Is(err, verdict.ErrAcceptedBlocking):
		return verdict.ExitCodeAcceptedBlocking
	case err != nil:
		parser.Errorf("%s", err)
		return exitFailed
	}
	return 0
}

// runInterruptible runs the parsed command under a context the operator's
// SIGINT or SIGTERM cancels: the first signal lets the command stop on its
// own terms (release locks, checkpoint, print its verdict), the second ends
// the process. Offline commands take no context and are unaffected. The
// signal handler is released when the command returns, before Main maps
// the error to an exit code.
func runInterruptible(kctx *kong.Context) error {
	ctx, stop := Interruptible(context.Background())
	defer stop()
	kctx.BindTo(ctx, (*context.Context)(nil))
	return kctx.Run()
}

// reportParseError writes the failing command's usage and the error to
// stderr. Kong prints usage on its stdout, so the parser's stdout is pointed
// at stderr before the usage is rendered.
func reportParseError(parser *kong.Kong, err error) int {
	var parseErr *kong.ParseError
	if errors.As(err, &parseErr) && parseErr.Context != nil {
		parser.Stdout = parser.Stderr
		if usageErr := parseErr.Context.PrintUsage(false); usageErr != nil {
			parser.Errorf("print usage: %s", usageErr)
		}
		_, _ = fmt.Fprintln(parser.Stderr)
	}
	parser.Errorf("%s", err)
	return exitFailed
}
