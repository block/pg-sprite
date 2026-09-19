package cli

import (
	"context"
	"errors"
	"io"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/diffplan"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

func (c *DiffCmd) runRowSecurityDiff(ctx context.Context, out io.Writer, sql string) error {
	desired, err := statement.ParseDesiredWithRowSecurity(sql)
	if err != nil {
		return errors.Join(verdict.ErrRefused, err)
	}
	pool, err := dbconn.NewPool(ctx, c.Config())
	if err != nil {
		return err
	}
	defer pool.Close()
	report, err := diffplan.PlanWithRowSecurity(ctx, pool, c.Schema, desired)
	if err != nil {
		if errors.Is(err, schemadiff.ErrUnsupportedChange) {
			return errors.Join(verdict.ErrRefused, err)
		}
		return err
	}
	return c.writeDiffReport(out, report)
}
