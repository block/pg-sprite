package checksum

import (
	"time"

	"github.com/block/pg-sprite/pkg/copier"
)

// VerifiedShadow proves a full checksum pass found source and shadow equal.
// Its zero value is forgeable; consumers must reject a proof whose Table is empty.
// Only a Check that compared every key, found no difference, and repaired
// nothing mints one (CO-1, CO-2).
type VerifiedShadow struct {
	schema, table, shadow string
	watermark             copier.Watermark
	verifiedAt            time.Time
}

func newVerifiedShadow(shadow copier.Shadow, watermark copier.Watermark, verifiedAt time.Time) VerifiedShadow {
	return VerifiedShadow{
		schema:     shadow.Schema(),
		table:      shadow.SourceTable(),
		shadow:     shadow.ShadowTable(),
		watermark:  watermark,
		verifiedAt: verifiedAt,
	}
}

// Schema returns the verified source schema.
func (v VerifiedShadow) Schema() string { return v.schema }

// Table returns the verified source table.
func (v VerifiedShadow) Table() string { return v.table }

// Shadow returns the verified shadow table.
func (v VerifiedShadow) Shadow() string { return v.shadow }

// Watermark returns the copied-through watermark at verification.
func (v VerifiedShadow) Watermark() copier.Watermark { return v.watermark }

// VerifiedAt returns the verification time.
func (v VerifiedShadow) VerifiedAt() time.Time { return v.verifiedAt }

// CleanWatermark proves every chunk through its watermark was clean on a fresh
// read and that the pass repaired nothing. Its zero value is forgeable;
// consumers must reject it when Watermark().Valid() is false. Only a Check
// that found no difference and repaired nothing mints one (CO-2).
type CleanWatermark struct{ watermark copier.Watermark }

func newCleanWatermark(watermark copier.Watermark) CleanWatermark {
	return CleanWatermark{watermark: watermark}
}

// Watermark returns the clean copied-through watermark.
func (w CleanWatermark) Watermark() copier.Watermark { return w.watermark }
