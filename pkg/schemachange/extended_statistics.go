package schemachange

import (
	"context"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
)

// ExtendedStatisticsTarget is one extended-statistics object's explicit
// ALTER STATISTICS … SET STATISTICS target. CREATE TABLE … LIKE INCLUDING
// ALL copies the object but leaves the copy at the server default, so the
// builder re-applies each explicit target; an object at the default is not
// recorded.
type ExtendedStatisticsTarget struct {
	// Name is the statistics object's unqualified name.
	Name string `json:"name"`
	// Target is the object's statistics target.
	Target int `json:"target"`
}

// readExtendedStatisticsTargets lists the table's extended-statistics
// objects whose target is set explicitly. The server stores the default as
// -1, or as NULL on servers whose catalog made the column nullable; both
// read as "default" and are left out.
func readExtendedStatisticsTargets(ctx context.Context, tx pgx.Tx, oid uint32) ([]ExtendedStatisticsTarget, error) {
	rows, err := tx.Query(ctx, `
		SELECT s.stxname, s.stxstattarget::int
		FROM pg_statistic_ext s
		WHERE s.stxrelid = $1 AND COALESCE(s.stxstattarget, -1) <> -1
		ORDER BY s.stxname`, oid)
	if err != nil {
		return nil, fmt.Errorf("read extended statistics targets: %w", err)
	}
	targets, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ExtendedStatisticsTarget, error) {
		var t ExtendedStatisticsTarget
		err := row.Scan(&t.Name, &t.Target)
		return t, err
	})
	if err != nil {
		return nil, fmt.Errorf("read extended statistics targets: %w", err)
	}
	return targets, nil
}

// carryExtendedStatisticsTargets sets each explicit source target on the
// shadow object with the same definition. LIKE names the copies after the
// shadow, so the two sides are paired by definition, as cutover pairs them
// (D8). It runs on the shadow LIKE just made, where every source object
// has a copy; a source object without one is a catalog the builder does
// not understand, and it fails rather than leave the target behind.
func carryExtendedStatisticsTargets(ctx context.Context, tx pgx.Tx, schema string, sourceOID, shadowOID uint32, targets []ExtendedStatisticsTarget) error {
	if len(targets) == 0 {
		return nil
	}
	source, err := readStatistics(ctx, tx, sourceOID)
	if err != nil {
		return err
	}
	shadow, err := readStatistics(ctx, tx, shadowOID)
	if err != nil {
		return err
	}
	shadowFor := make(map[string]string, len(source))
	for _, pair := range pairByDefinition(DependentStatistics, source, shadow).Pairs {
		shadowFor[pair.SourceName] = pair.ShadowName
	}
	for _, t := range targets {
		name, paired := shadowFor[t.Name]
		if !paired {
			return fmt.Errorf("source statistics object %s has no shadow counterpart to carry its target to", t.Name)
		}
		sql := "ALTER STATISTICS " + pgx.Identifier{schema, name}.Sanitize() + " SET STATISTICS " + strconv.Itoa(t.Target)
		if _, err := tx.Exec(ctx, sql); err != nil {
			return fmt.Errorf("set statistics target of shadow statistics object %s for %s: %w", name, t.Name, err)
		}
	}
	return nil
}
