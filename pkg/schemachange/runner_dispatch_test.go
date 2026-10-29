package schemachange

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// convertingProof is the proof of a shadow that retypes qty through a USING
// expression, with the conversion fingerprint the build read.
func convertingProof(fingerprint string) Proof {
	return Proof{
		Schema:                "app",
		SourceTable:           "orders",
		ShadowTable:           "_orders_shadow",
		SourceOID:             16384,
		ShadowOID:             16400,
		SourceFingerprint:     "source",
		TargetFingerprint:     "target",
		CopyColumns:           []string{"id", "qty"},
		Conversions:           map[string]string{"qty": "(qty * 1)::bigint"},
		ConversionFingerprint: fingerprint,
	}
}

// A row whose proof differs from the re-derived one in its conversion
// fingerprint alone describes the shadow the build made over a catalog
// that changed under its conversions: the copy's own drift.
func TestOnlyConversionsDriftedWhenTheFingerprintAloneDiffers(t *testing.T) {
	drifted, err := onlyConversionsDrifted(convertingProof("sha256:build"), convertingProof("sha256:now"))
	require.NoError(t, err)
	assert.True(t, drifted)
}

// A row written without a fingerprint, by an engine that recorded none,
// differs from the re-derived proof because of the row, not the catalog,
// so it is not drift.
func TestOnlyConversionsDriftedIsFalseForARowWithoutAFingerprint(t *testing.T) {
	drifted, err := onlyConversionsDrifted(convertingProof(""), convertingProof("sha256:now"))
	require.NoError(t, err)
	assert.False(t, drifted)
}

// A row whose proof differs in any other field as well describes another
// shadow, whatever its fingerprint says.
func TestOnlyConversionsDriftedIsFalseWhenAnotherFieldDiffers(t *testing.T) {
	derived := convertingProof("sha256:now")
	derived.ShadowOID = 16500
	drifted, err := onlyConversionsDrifted(convertingProof("sha256:build"), derived)
	require.NoError(t, err)
	assert.False(t, drifted)
}
