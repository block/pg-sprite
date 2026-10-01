package preflight

import (
	"fmt"
	"hash/fnv"
)

// copySwapDecodingPrefix marks the replication slot and the publication the
// copy-and-swap route creates, so an operator can list the engine's
// logical-decoding state by prefix.
const copySwapDecodingPrefix = "pgsprite_"

// CopySwapDecodingName is the name the copy-and-swap route gives both the
// replication slot and the single-table publication it creates for a
// table: the engine prefix followed by eight hex digits of an FNV-1a hash
// of database.schema.table. A replication slot is cluster-wide while a
// table is per-database, so the database is part of the hash and two
// databases holding a same-named table derive different slots. The same
// table always derives the same name, so a resumed run finds the slot it
// created and the preflight knows which publication is the engine's own.
func CopySwapDecodingName(database, schema, table string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(database + "." + schema + "." + table))
	return fmt.Sprintf("%s%08x", copySwapDecodingPrefix, h.Sum32())
}
