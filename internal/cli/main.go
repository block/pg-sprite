package cli

import (
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

// Main parses args, runs the selected command, and returns the process exit
// code. stdout is the destination for --help and --version output only;
// commands write their results to the process stdout themselves. A command
// line that does not parse is reported on stderr — never stdout, which
// under --json carries only the machine-readable product — and exits
// exitFailed.
func Main(args []string, version string, stdout, stderr io.Writer) int {
	parser, err := kong.New(New(version),
		kong.Name("pg-sprite"),
		kong.Description("An online schema-change engine for PostgreSQL."),
		kong.Writers(stdout, stderr),
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

	err = kctx.Run()
	// A refusal verdict was already printed; its exit code is distinct from
	// operational errors so automation can branch on the difference.
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
