package checksum

import (
	"time"

	"github.com/block/pg-sprite/pkg/copier"
)

// VerifiedShadow proves a full checksum pass found source and shadow equal.
// Its zero value is forgeable; consumers must reject a proof whose Table is empty.
// Only a pass that compared every key, found no difference, and repaired
// nothing mints one (CO-1, CO-2): Check, over a quiesced source, and
// CheckLive, over a source the stream is carrying to the shadow. The proof
// says which (Live), because the two do not mean the same thing to the
// swap: a quiesced proof is the shadow equal to the source as it stands,
// and a live proof is the shadow equal to the source as of the position
// the pass settled against, with every change since in the stream, which
// the swap must drain under its lock before it renames. It names the two
// relations by OID as well as by name: the shadow's name is derived from
// the source's, so a shadow rebuilt after this proof was minted wears the
// same name while holding none of the rows the pass compared.
type VerifiedShadow struct {
	schema, table, shadow string
	sourceOID, shadowOID  uint32
	watermark             copier.Watermark
	verifiedAt            time.Time
	live                  bool
}

func newVerifiedShadow(shadow copier.Shadow, watermark copier.Watermark, verifiedAt time.Time, live bool) VerifiedShadow {
	return VerifiedShadow{
		schema:     shadow.Schema(),
		table:      shadow.SourceTable(),
		shadow:     shadow.ShadowTable(),
		sourceOID:  shadow.SourceOID(),
		shadowOID:  shadow.ShadowOID(),
		watermark:  watermark,
		verifiedAt: verifiedAt,
		live:       live,
	}
}

// Schema returns the verified source schema.
func (v VerifiedShadow) Schema() string { return v.schema }

// Table returns the verified source table.
func (v VerifiedShadow) Table() string { return v.table }

// Shadow returns the verified shadow table.
func (v VerifiedShadow) Shadow() string { return v.shadow }

// SourceOID returns pg_class.oid of the source relation the pass compared.
func (v VerifiedShadow) SourceOID() uint32 { return v.sourceOID }

// ShadowOID returns pg_class.oid of the shadow relation the pass compared.
func (v VerifiedShadow) ShadowOID() uint32 { return v.shadowOID }

// Watermark returns the copied-through watermark at verification.
func (v VerifiedShadow) Watermark() copier.Watermark { return v.watermark }

// VerifiedAt returns the verification time.
func (v VerifiedShadow) VerifiedAt() time.Time { return v.verifiedAt }

// Live reports whether the pass that minted the proof ran while the stream
// was applying the source's changes to the shadow. A live proof holds only
// up to the position the pass settled against; the swap that acts on it
// must drain the stream's remaining changes under its lock before it
// renames, and the cutover refuses a live proof with no drain.
func (v VerifiedShadow) Live() bool { return v.live }

// CleanWatermark proves every chunk through its watermark was clean on a fresh
// read and that the pass repaired nothing. Its zero value is forgeable;
// consumers must reject it when Watermark().Valid() is false. Only a pass
// (Check or CheckLive) that found no difference and repaired nothing mints
// one (CO-2).
type CleanWatermark struct{ watermark copier.Watermark }

func newCleanWatermark(watermark copier.Watermark) CleanWatermark {
	return CleanWatermark{watermark: watermark}
}

// Watermark returns the clean copied-through watermark.
func (w CleanWatermark) Watermark() copier.Watermark { return w.watermark }
