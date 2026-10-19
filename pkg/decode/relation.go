package decode

import (
	"errors"
	"fmt"

	"github.com/jackc/pglogrepl"
)

// ErrSourceShapeChanged is returned when pgoutput describes the target table
// differently from how the stream first saw it: a column added, dropped,
// renamed, or retyped, or the replica identity changed. The decoded columns
// would no longer line up with the shadow, so the stream stops rather than
// guess; the route's resume path decides what to do with the shadow.
var ErrSourceShapeChanged = errors.New("the source table's shape changed under the stream")

// keyFlag marks a column pgoutput reports as part of the replica identity.
const keyFlag uint8 = 1

// relation is the stream's record of the target table as pgoutput described
// it: the columns in tuple order and which of them carries the primary key.
type relation struct {
	id              uint32
	replicaIdentity uint8
	columns         []pglogrepl.RelationMessageColumn
	keyIndex        int
}

// relationTarget is what a relation message is checked against: the names
// and the catalog OID the target was proven for, and the column its key is
// expected in. preflight.CopySwapTarget is the one the stream carries.
type relationTarget interface {
	Schema() string
	Table() string
	OID() uint32
	PKColumn() string
}

// newRelation checks that a relation message describes the target table and
// records it. The message must name the target — the publication publishes
// nothing else, so any other relation is state this package did not create —
// and must carry the OID the target was proven under: pgoutput reports the
// table's OID with its name, and a table dropped and recreated under the
// same name is another table, which the names alone cannot tell apart. The
// message must also flag the target's primary-key column as part of the
// key, which is what makes a key-only old tuple carry it.
func newRelation(msg *pglogrepl.RelationMessage, target relationTarget) (*relation, error) {
	if msg.Namespace != target.Schema() || msg.RelationName != target.Table() {
		return nil, fmt.Errorf("%w: ST-3: stream for %s.%s decoded relation %s.%s",
			ErrInvariantViolation, target.Schema(), target.Table(), msg.Namespace, msg.RelationName)
	}
	if msg.RelationID != target.OID() {
		return nil, fmt.Errorf("%w: ST-3: stream for %s.%s, OID %d, decoded a relation of that name with OID %d: not the table the target was proven for",
			ErrInvariantViolation, target.Schema(), target.Table(), target.OID(), msg.RelationID)
	}
	r := &relation{id: msg.RelationID, replicaIdentity: msg.ReplicaIdentity, keyIndex: -1}
	for i, c := range msg.Columns {
		r.columns = append(r.columns, *c)
		if c.Name == target.PKColumn() {
			r.keyIndex = i
		}
	}
	if r.keyIndex < 0 {
		return nil, fmt.Errorf("%w: ST-3: relation %s.%s has no column %s",
			ErrInvariantViolation, msg.Namespace, msg.RelationName, target.PKColumn())
	}
	if r.columns[r.keyIndex].Flags&keyFlag == 0 {
		return nil, fmt.Errorf("%w: ST-3: column %s of %s.%s is not part of the replica identity",
			ErrInvariantViolation, target.PKColumn(), msg.Namespace, msg.RelationName)
	}
	return r, nil
}

// sameShape reports whether another description agrees with this one: the
// same relation, column for column — name, type, type modifier, and key
// flag — under the same replica identity.
func (r *relation) sameShape(other *relation) bool {
	if r.id != other.id || r.replicaIdentity != other.replicaIdentity || len(r.columns) != len(other.columns) {
		return false
	}
	for i := range r.columns {
		if r.columns[i] != other.columns[i] {
			return false
		}
	}
	return true
}
