package testutil

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// RelationRef identifies a relation.
type RelationRef struct {
	Schema string
	Table  string
}

// ConvergeOptions controls convergence comparison.
type ConvergeOptions struct{ IgnoreColumns []string }

// DirectionDiff reports differing primary keys in one EXCEPT direction.
type DirectionDiff struct {
	Direction string
	Keys      []int64
}

// Report contains row counts and bounded symmetric differences.
type Report struct {
	SourceCount int64
	ShadowCount int64
	Differences []DirectionDiff
}

// Converged reports whether counts and row values match.
func (r Report) Converged() bool { return r.SourceCount == r.ShadowCount && len(r.Differences) == 0 }

type compareColumn struct{ Name, Type string }

// Diff compares source against the shadow's visible column contract.
func Diff(ctx context.Context, pool *pgxpool.Pool, source, shadow RelationRef, opts ConvergeOptions) (Report, error) {
	columns, err := shadowColumns(ctx, pool, shadow, opts)
	if err != nil {
		return Report{}, err
	}
	if len(columns) == 0 {
		return Report{}, fmt.Errorf("shadow %s.%s has no comparable columns", shadow.Schema, shadow.Table)
	}
	sourceNames, err := relationColumnNames(ctx, pool, source)
	if err != nil {
		return Report{}, err
	}
	for _, c := range columns {
		if !sourceNames[c.Name] {
			return Report{}, fmt.Errorf("source %s.%s lacks shadow column %s", source.Schema, source.Table, c.Name)
		}
	}
	var report Report
	if err = pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM `+qualified(source)+`), (SELECT count(*) FROM `+qualified(shadow)+`)`).Scan(&report.SourceCount, &report.ShadowCount); err != nil {
		return Report{}, fmt.Errorf("count convergence relations: %w", err)
	}
	selectSource, selectShadow := make([]string, 0, len(columns)), make([]string, 0, len(columns))
	for _, c := range columns {
		id := pgx.Identifier{c.Name}.Sanitize()
		selectSource = append(selectSource, id+`::`+c.Type)
		selectShadow = append(selectShadow, id)
	}
	for _, direction := range []struct{ name, left, right string }{{"source-minus-shadow", strings.Join(selectSource, ","), strings.Join(selectShadow, ",")}, {"shadow-minus-source", strings.Join(selectShadow, ","), strings.Join(selectSource, ",")}} {
		leftRel, rightRel := source, shadow
		if direction.name == "shadow-minus-source" {
			leftRel, rightRel = shadow, source
		}
		q := `SELECT id FROM (SELECT ` + direction.left + ` FROM ` + qualified(leftRel) + ` EXCEPT ALL SELECT ` + direction.right + ` FROM ` + qualified(rightRel) + `) d ORDER BY id LIMIT 20`
		rows, e := pool.Query(ctx, q)
		if e != nil {
			return Report{}, fmt.Errorf("query %s difference: %w", direction.name, e)
		}
		keys, e := pgx.CollectRows(rows, pgx.RowTo[int64])
		if e != nil {
			return Report{}, fmt.Errorf("read %s difference: %w", direction.name, e)
		}
		if len(keys) > 0 {
			report.Differences = append(report.Differences, DirectionDiff{Direction: direction.name, Keys: keys})
		}
	}
	return report, nil
}
func qualified(r RelationRef) string { return pgx.Identifier{r.Schema, r.Table}.Sanitize() }
func relationColumnNames(ctx context.Context, pool *pgxpool.Pool, r RelationRef) (map[string]bool, error) {
	rows, err := pool.Query(ctx, `SELECT column_name FROM information_schema.columns WHERE table_schema=$1 AND table_name=$2`, r.Schema, r.Table)
	if err != nil {
		return nil, fmt.Errorf("introspect %s.%s columns: %w", r.Schema, r.Table, err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	result := make(map[string]bool, len(names))
	for _, name := range names {
		result[name] = true
	}
	return result, nil
}
func shadowColumns(ctx context.Context, pool *pgxpool.Pool, r RelationRef, opts ConvergeOptions) ([]compareColumn, error) {
	ignored := make(map[string]bool, len(opts.IgnoreColumns))
	for _, name := range opts.IgnoreColumns {
		ignored[name] = true
	}
	rows, err := pool.Query(ctx, `SELECT a.attname, format_type(a.atttypid,a.atttypmod) FROM pg_attribute a JOIN pg_class c ON c.oid=a.attrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND c.relname=$2 AND a.attnum>0 AND NOT a.attisdropped ORDER BY a.attnum`, r.Schema, r.Table)
	if err != nil {
		return nil, fmt.Errorf("introspect shadow %s.%s: %w", r.Schema, r.Table, err)
	}
	values, err := pgx.CollectRows(rows, pgx.RowToStructByPos[compareColumn])
	if err != nil {
		return nil, err
	}
	result := values[:0]
	for _, c := range values {
		if !ignored[c.Name] {
			result = append(result, c)
		}
	}
	return result, nil
}

// AssertConverged requires that source and shadow contain equal rows.
func AssertConverged(t *testing.T, ctx context.Context, pool *pgxpool.Pool, source, shadow RelationRef, opts ConvergeOptions) {
	t.Helper()
	report, err := Diff(ctx, pool, source, shadow, opts)
	require.NoError(t, err)
	assert.Equal(t, report.SourceCount, report.ShadowCount, "row counts differ")
	assert.Empty(t, report.Differences, "relations differ by direction and primary keys: %+v", report.Differences)
}
