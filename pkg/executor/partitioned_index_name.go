package executor

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"unicode/utf8"
)

// maxIdentifierBytes is PostgreSQL's identifier length limit. The server
// silently truncates a longer name, so a derived name has to fit it or the
// index would not carry the name the build looks it up under.
const maxIdentifierBytes = 63

// nameDigestHexLen is how many hex characters of the parent index name's
// digest a cut derived name carries.
const nameDigestHexLen = 8

// derivedPartitionIndexName is the name the build gives the index it creates
// on a partition: the parent index's name, "_p", then the partition's OID. It
// is deterministic, so a rerun finds the index an earlier run built, and
// unique per partition because OIDs are.
//
// When the whole name would pass the identifier limit, the parent's name is
// cut at a character boundary and a digest of the whole parent name goes
// between it and the suffix: "<cut>_<digest>_p<oid>". Two parent indexes on one
// table that share their first characters would otherwise cut to the same
// text. A name that fits is left as it is.
func derivedPartitionIndexName(parentIndex string, partitionOID uint32) string {
	suffix := "_p" + strconv.FormatUint(uint64(partitionOID), 10)
	if len(parentIndex)+len(suffix) <= maxIdentifierBytes {
		return parentIndex + suffix
	}
	digest := sha256.Sum256([]byte(parentIndex))
	mark := "_" + hex.EncodeToString(digest[:])[:nameDigestHexLen]
	cut := maxIdentifierBytes - len(suffix) - len(mark)
	for cut > 0 && !utf8.RuneStart(parentIndex[cut]) {
		cut--
	}
	return parentIndex[:cut] + mark + suffix
}
