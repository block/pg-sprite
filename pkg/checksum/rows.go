package checksum

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
)

// rowHash is one row's SHA-256 as the digest statement computes it. The
// zero value stands for a key the side holds no row for, so a key present
// on one side and absent on the other compares unequal, and two absences
// compare equal; no row hashes to all zero bytes.
type rowHash [sha256.Size]byte

// keyedRow is one row of a chunk on one side: its key and its hash.
type keyedRow struct {
	key  int64
	hash rowHash
}

// rowSet is every row one side holds in a chunk, in ascending key order,
// as the rows statement returns them.
type rowSet []keyedRow

// at returns the hash of the row with key, or the zero hash when the side
// holds no row with it.
func (s rowSet) at(key int64) rowHash {
	i, found := slices.BinarySearchFunc(s, key, func(r keyedRow, k int64) int {
		return cmp.Compare(r.key, k)
	})
	if !found {
		return rowHash{}
	}
	return s[i].hash
}

// digest is the set's chunk digest, computed as the digest statement
// computes it: the row count and the hex SHA-256 of the hashes concatenated
// in key order, the SHA-256 of the empty input for an empty set.
func (s rowSet) digest() Digest {
	h := sha256.New()
	for _, r := range s {
		h.Write(r.hash[:])
	}
	return Digest{Rows: int64(len(s)), Hash: hex.EncodeToString(h.Sum(nil))}
}

// confirmDigest checks the chunk digest the server aggregated against the
// one derived from the rows it returned in the same snapshot. The two
// statements hash every row the same way, so a disagreement means the pass's
// two renderings of one chunk do not describe the same rows, and nothing
// judged from them could be trusted (CO-1).
func confirmDigest(side string, server Digest, rows rowSet) error {
	derived := rows.digest()
	if derived == server {
		return nil
	}
	// INV: CO-1
	return fmt.Errorf("%w (CO-1): %s rows of the chunk digest as %d rows %s, the chunk's digest is %d rows %s", ErrInvariantViolation, side, derived.Rows, derived.Hash, server.Rows, server.Hash)
}

// readRows runs one side's rows statement with args — the closed range
// [lower, upper] of a chunk, or the list of keys of a keyed read — and
// returns every row it selects, in key order. A hash that is not a SHA-256
// is a statement that is not one this package renders.
func readRows(ctx context.Context, tx pgx.Tx, sql string, args ...any) (rowSet, error) {
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (keyedRow, error) {
		var (
			r    keyedRow
			hash []byte
		)
		if err := row.Scan(&r.key, &hash); err != nil {
			return keyedRow{}, err
		}
		if len(hash) != sha256.Size {
			// INV: CO-1
			return keyedRow{}, fmt.Errorf("%w (CO-1): row %d hashed to %d bytes, not a SHA-256", ErrInvariantViolation, r.key, len(hash))
		}
		copy(r.hash[:], hash)
		return r, nil
	})
}

// unionKeys returns every key any of the sets holds, each once, in
// ascending order.
func unionKeys(sets ...rowSet) []int64 {
	var keys []int64
	for _, s := range sets {
		for _, r := range s {
			keys = append(keys, r.key)
		}
	}
	slices.Sort(keys)
	return slices.Compact(keys)
}
