package preflight

import (
	"fmt"
	"hash/fnv"

	"github.com/jackc/pgx/v5"
)

// copySwapDecodingPrefix marks the replication slot and the publication the
// copy-and-swap route creates, so an operator can list the engine's
// logical-decoding state by prefix.
const copySwapDecodingPrefix = "pgsprite_"

// CopySwapDecodingName is the name the copy-and-swap route gives both the
// replication slot and the single-table publication it creates for a
// table: the engine prefix followed by eight hex digits of an FNV-1a hash
// of the quoted, dotted database.schema.table. Quoting keeps the three
// names apart however they are spelled — a dot inside one name cannot make
// two tables read as one. A replication slot is cluster-wide while a table
// is per-database, so the database is part of the hash and two databases
// holding a same-named table derive different slots. The same table always
// derives the same name, so a resumed run finds the slot it created and the
// preflight knows which publication is the engine's own.
func CopySwapDecodingName(database, schema, table string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(pgx.Identifier{database, schema, table}.Sanitize()))
	return fmt.Sprintf("%s%08x", copySwapDecodingPrefix, h.Sum32())
}
