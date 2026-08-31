package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/verdict"
)

// ErrPullFailed is returned when at least one table could not be
// introspected or written. Render refusals use verdict.ErrRefused instead.
var ErrPullFailed = errors.New("one or more tables could not be pulled")

type pullStatus string

const (
	pullStatusPulled  pullStatus = "pulled"
	pullStatusRefused pullStatus = "refused"
	pullStatusError   pullStatus = "error"
)

type pullResult struct {
	table  string
	path   string
	status pullStatus
	err    error
}

type tablePuller func(context.Context, *pgxpool.Pool, string, string, string) error

func (c *PullCmd) run(ctx context.Context, out io.Writer) error {
	pool, err := dbconn.NewPool(ctx, c.Config())
	if err != nil {
		return err
	}
	defer pool.Close()

	var schemaExists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_namespace WHERE nspname = $1)`, c.Schema).Scan(&schemaExists); err != nil {
		return fmt.Errorf("check schema %s: %w", c.Schema, err)
	}
	if !schemaExists {
		return fmt.Errorf("schema %q does not exist", c.Schema)
	}
	tables, err := listTables(ctx, pool, c.Schema)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(c.Out, 0o755); err != nil {
		return fmt.Errorf("create output directory %s: %w", c.Out, err)
	}
	results := pullTables(ctx, pool, c.Schema, c.Out, tables, pullOneTable)
	if err := writePullText(out, results); err != nil {
		return err
	}
	return pullResultsError(results)
}

func listTables(ctx context.Context, pool *pgxpool.Pool, schema string) ([]string, error) {
	rows, err := pool.Query(ctx, `
		SELECT c.relname
		FROM pg_catalog.pg_class c
		JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relkind IN ('r', 'p')
		  AND NOT c.relispartition
		  AND NOT EXISTS (
		      SELECT 1 FROM pg_catalog.pg_depend d
		      WHERE d.classid = 'pg_class'::regclass
		        AND d.objid = c.oid AND d.deptype = 'e'
		  )
		ORDER BY c.relname`, schema)
	if err != nil {
		return nil, fmt.Errorf("list tables in schema %s: %w", schema, err)
	}
	defer rows.Close()
	var tables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			return nil, fmt.Errorf("scan table in schema %s: %w", schema, err)
		}
		tables = append(tables, table)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read tables in schema %s: %w", schema, err)
	}
	return tables, nil
}

func pullTables(ctx context.Context, pool *pgxpool.Pool, schema, outDir string, tables []string, pull tablePuller) []pullResult {
	results := make([]pullResult, 0, len(tables))
	seenPaths := make(map[string]string, len(tables))
	for _, table := range tables {
		if err := ctx.Err(); err != nil {
			results = append(results, pullResult{table: table, status: pullStatusError, err: err})
			break
		}
		path, err := tableOutputPath(outDir, table)
		if err != nil {
			results = append(results, pullResult{table: table, status: pullStatusError, err: err})
			continue
		}
		pathKey := strings.ToLower(filepath.Base(path))
		if other, ok := seenPaths[pathKey]; ok {
			results = append(results, pullResult{
				table: table, status: pullStatusError,
				err: fmt.Errorf("output file name has a case collision with table %q", other),
			})
			continue
		}
		seenPaths[pathKey] = table
		err = pull(ctx, pool, schema, table, path)
		result := pullResult{table: table, path: path, status: pullStatusPulled}
		if err != nil {
			result.status = pullStatusError
			result.err = err
			var refusal *renderRefusal
			if errors.As(err, &refusal) {
				result.status = pullStatusRefused
			}
		}
		results = append(results, result)
	}
	return results
}

func tableOutputPath(outDir, table string) (string, error) {
	name := table + ".sql"
	if filepath.Base(name) != name || name == ".sql" {
		return "", fmt.Errorf("table %q cannot be represented as a safe file name", table)
	}
	return filepath.Join(outDir, name), nil
}

type renderRefusal struct{ err error }

func (e *renderRefusal) Error() string { return e.err.Error() }
func (e *renderRefusal) Unwrap() error { return e.err }

func pullOneTable(ctx context.Context, pool *pgxpool.Pool, schema, table, path string) error {
	model, err := schemadiff.Introspect(ctx, pool, schema, table)
	if err != nil {
		return fmt.Errorf("introspect %s.%s: %w", schema, table, err)
	}
	rendered, err := schemadiff.Render(model)
	if err != nil {
		return &renderRefusal{err: err}
	}
	return pullRenderedFile(path, rendered)
}

func pullRenderedFile(path, rendered string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	if _, err := io.WriteString(file, rendered); err != nil {
		closeErr := file.Close()
		removeErr := os.Remove(path)
		return errors.Join(fmt.Errorf("write %s: %w", path, err), closeErr, removeErr)
	}
	if err := file.Close(); err != nil {
		removeErr := os.Remove(path)
		return errors.Join(fmt.Errorf("close %s: %w", path, err), removeErr)
	}
	return nil
}

func writePullText(out io.Writer, results []pullResult) error {
	counts := map[pullStatus]int{}
	for _, result := range results {
		var err error
		switch result.status {
		case pullStatusPulled:
			counts[result.status]++
			_, err = fmt.Fprintf(out, "PULLED  %s -> %s\n", result.table, result.path)
		case pullStatusRefused:
			counts[result.status]++
			_, err = fmt.Fprintf(out, "REFUSED %s: %v\n", result.table, result.err)
		case pullStatusError:
			counts[result.status]++
			_, err = fmt.Fprintf(out, "ERROR   %s: %v\n", result.table, result.err)
		default:
			return fmt.Errorf("write pull report: unexpected pull status %q for table %q", result.status, result.table)
		}
		if err != nil {
			return fmt.Errorf("write pull report: %w", err)
		}
	}
	if _, err := fmt.Fprintf(out, "Summary: %d pulled, %d refused, %d errors\n",
		counts[pullStatusPulled], counts[pullStatusRefused], counts[pullStatusError]); err != nil {
		return fmt.Errorf("write pull report: %w", err)
	}
	return nil
}

func pullResultsError(results []pullResult) error {
	refused := false
	for _, result := range results {
		if result.status == pullStatusError {
			return ErrPullFailed
		}
		refused = refused || result.status == pullStatusRefused
	}
	if refused {
		return verdict.ErrRefused
	}
	return nil
}
