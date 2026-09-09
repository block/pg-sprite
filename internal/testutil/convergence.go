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
	key, err := primaryKeyColumn(ctx, pool, shadow)
	if err != nil {
		return Report{}, err
	}
	if !containsColumn(columns, key) {
		return Report{}, fmt.Errorf("shadow %s.%s primary key %s cannot be ignored", shadow.Schema, shadow.Table, key)
	}
	var report Report
	if err = pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM `+qualified(source)+`), (SELECT count(*) FROM `+qualified(shadow)+`)`).Scan(&report.SourceCount, &report.ShadowCount); err != nil {
		return Report{}, fmt.Errorf("count convergence relations: %w", err)
	}
	// The source side casts every column to the shadow's type so a widened
	// or narrowed column compares by value, not by representation.
	selectSource, selectShadow := make([]string, 0, len(columns)), make([]string, 0, len(columns))
	for _, c := range columns {
		id := pgx.Identifier{c.Name}.Sanitize()
		selectSource = append(selectSource, id+`::`+c.Type)
		selectShadow = append(selectShadow, id)
	}
	sourceSide := relationSide{rel: source, columns: strings.Join(selectSource, ",")}
	shadowSide := relationSide{rel: shadow, columns: strings.Join(selectShadow, ",")}
	directions := []struct {
		name        string
		left, right relationSide
	}{
		{"source-minus-shadow", sourceSide, shadowSide},
		{"shadow-minus-source", shadowSide, sourceSide},
	}
	for _, d := range directions {
		keys, err := missingKeys(ctx, pool, d.left, d.right, key)
		if err != nil {
			return Report{}, fmt.Errorf("%s difference: %w", d.name, err)
		}
		if len(keys) > 0 {
			report.Differences = append(report.Differences, DirectionDiff{Direction: d.name, Keys: keys})
		}
	}
	return report, nil
}

// relationSide is one operand of an EXCEPT: the relation and its projected,
// already-cast column list.
type relationSide struct {
	rel     RelationRef
	columns string
}

// missingKeys returns up to the first differenceLimit primary keys of rows
// present in left but absent from right (EXCEPT ALL, so duplicate rows
// count).
func missingKeys(ctx context.Context, pool *pgxpool.Pool, left, right relationSide, key string) ([]int64, error) {
	keyID := pgx.Identifier{key}.Sanitize()
	q := `SELECT ` + keyID + ` FROM (SELECT ` + left.columns + ` FROM ` + qualified(left.rel) +
		` EXCEPT ALL SELECT ` + right.columns + ` FROM ` + qualified(right.rel) +
		`) d ORDER BY ` + keyID + ` LIMIT ` + fmt.Sprint(differenceLimit)
	rows, err := pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	keys, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	return keys, nil
}

// differenceLimit bounds how many differing keys one direction reports; a
// diverged table is diagnosed from its first rows, not enumerated.
const differenceLimit = 20

func containsColumn(columns []compareColumn, name string) bool {
	for _, c := range columns {
		if c.Name == name {
			return true
		}
	}
	return false
}

// primaryKeyColumn returns the shadow's single primary-key column, the key
// the report names differing rows by. The copy path supports only
// single-column integer keys, so a composite key is an error here too.
func primaryKeyColumn(ctx context.Context, pool *pgxpool.Pool, r RelationRef) (string, error) {
	rows, err := pool.Query(ctx, `SELECT a.attname FROM pg_index i JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = ANY (i.indkey) WHERE i.indrelid = $1::regclass AND i.indisprimary ORDER BY a.attnum`, qualified(r))
	if err != nil {
		return "", fmt.Errorf("introspect %s.%s primary key: %w", r.Schema, r.Table, err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return "", fmt.Errorf("read %s.%s primary key: %w", r.Schema, r.Table, err)
	}
	if len(names) != 1 {
		return "", fmt.Errorf("%s.%s must have a single-column primary key, found %d key columns", r.Schema, r.Table, len(names))
	}
	return names[0], nil
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
