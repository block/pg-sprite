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

// Report contains row counts and bounded symmetric differences, all read
// from one REPEATABLE READ snapshot so the numbers describe one instant.
type Report struct {
	SourceCount int64
	ShadowCount int64
	Differences []DirectionDiff
}

// Converged reports whether the relations hold equal rows. The counts are
// diagnostic only: the primary key is always projected and both counts come
// from the same snapshot as the differences, so any count skew necessarily
// surfaces as a differing key.
func (r Report) Converged() bool { return len(r.Differences) == 0 }

type compareColumn struct{ Name, Type string }

// Diff compares source against the shadow's visible column contract.
//
// Every catalog read, both counts, and both difference queries run in one
// read-only REPEATABLE READ transaction, so a Report is a single snapshot:
// a count difference and a row difference can never disagree about the
// instant they describe.
func Diff(ctx context.Context, pool *pgxpool.Pool, source, shadow RelationRef, opts ConvergeOptions) (Report, error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return Report{}, fmt.Errorf("begin convergence snapshot: %w", err)
	}
	// The snapshot is released by Commit on success; the deferred rollback
	// is the safety closer for the error path and reports already-closed
	// after a commit.
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	report, err := diffInSnapshot(ctx, tx, source, shadow, opts)
	if err != nil {
		return Report{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Report{}, fmt.Errorf("release convergence snapshot: %w", err)
	}
	return report, nil
}

func diffInSnapshot(ctx context.Context, tx pgx.Tx, source, shadow RelationRef, opts ConvergeOptions) (Report, error) {
	columns, err := shadowColumns(ctx, tx, shadow, opts)
	if err != nil {
		return Report{}, err
	}
	if len(columns) == 0 {
		return Report{}, fmt.Errorf("shadow %s.%s has no comparable columns", shadow.Schema, shadow.Table)
	}
	sourceNames, err := relationColumnNames(ctx, tx, source)
	if err != nil {
		return Report{}, err
	}
	for _, c := range columns {
		if !sourceNames[c.Name] {
			return Report{}, fmt.Errorf("source %s.%s lacks shadow column %s", source.Schema, source.Table, c.Name)
		}
	}
	key, err := primaryKeyColumn(ctx, tx, shadow)
	if err != nil {
		return Report{}, err
	}
	if !containsColumn(columns, key) {
		return Report{}, fmt.Errorf("shadow %s.%s primary key %s cannot be ignored", shadow.Schema, shadow.Table, key)
	}
	var report Report
	if err = tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM `+qualified(source)+`), (SELECT count(*) FROM `+qualified(shadow)+`)`).Scan(&report.SourceCount, &report.ShadowCount); err != nil {
		return Report{}, fmt.Errorf("count convergence relations: %w", err)
	}
	// The source side casts every column to the shadow's type: a widened or
	// narrowed column compares by value, not by representation, and a column
	// whose type moved to another category (numeric to text, say) can be
	// matched by EXCEPT at all.
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
		keys, err := missingKeys(ctx, tx, d.left, d.right, key)
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

// missingKeys returns the lowest DifferenceLimit primary keys of rows
// present in left but absent from right. The primary key is always in the
// projection, so every projected row is already distinct and EXCEPT loses
// no multiplicity.
func missingKeys(ctx context.Context, tx pgx.Tx, left, right relationSide, key string) ([]int64, error) {
	keyID := pgx.Identifier{key}.Sanitize()
	q := `SELECT ` + keyID + ` FROM (SELECT ` + left.columns + ` FROM ` + qualified(left.rel) +
		` EXCEPT SELECT ` + right.columns + ` FROM ` + qualified(right.rel) +
		`) d ORDER BY ` + keyID + ` LIMIT ` + fmt.Sprint(DifferenceLimit)
	rows, err := tx.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	keys, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	return keys, nil
}

// DifferenceLimit bounds how many differing keys one direction reports; a
// diverged table is diagnosed from its lowest keys, not enumerated.
const DifferenceLimit = 20

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
func primaryKeyColumn(ctx context.Context, tx pgx.Tx, r RelationRef) (string, error) {
	rows, err := tx.Query(ctx, `SELECT a.attname FROM pg_index i JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = ANY (i.indkey) WHERE i.indrelid = $1::regclass AND i.indisprimary ORDER BY a.attnum`, qualified(r))
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
func relationColumnNames(ctx context.Context, tx pgx.Tx, r RelationRef) (map[string]bool, error) {
	rows, err := tx.Query(ctx, `SELECT column_name FROM information_schema.columns WHERE table_schema=$1 AND table_name=$2`, r.Schema, r.Table)
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
func shadowColumns(ctx context.Context, tx pgx.Tx, r RelationRef, opts ConvergeOptions) ([]compareColumn, error) {
	ignored := make(map[string]bool, len(opts.IgnoreColumns))
	for _, name := range opts.IgnoreColumns {
		ignored[name] = true
	}
	rows, err := tx.Query(ctx, `SELECT a.attname, format_type(a.atttypid,a.atttypmod) FROM pg_attribute a JOIN pg_class c ON c.oid=a.attrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND c.relname=$2 AND a.attnum>0 AND NOT a.attisdropped ORDER BY a.attnum`, r.Schema, r.Table)
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
func AssertConverged(t *testing.T, pool *pgxpool.Pool, source, shadow RelationRef, opts ConvergeOptions) {
	t.Helper()
	report, err := Diff(t.Context(), pool, source, shadow, opts)
	require.NoError(t, err)
	assert.Empty(t, report.Differences, "relations differ (source %d rows, shadow %d rows) by direction and primary keys: %+v", report.SourceCount, report.ShadowCount, report.Differences)
}
