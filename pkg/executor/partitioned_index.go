// The partitioned index build is the online way to add an index to a
// partitioned table. PostgreSQL cannot build one concurrently, and a plain
// CREATE INDEX on a partitioned table blocks writes to the whole tree. The
// documented alternative is three steps: create the parent's index with ONLY,
// which builds nothing; build each partition's index concurrently; attach each
// one. This executor runs them partition by partition
// (docs/partitioned-index-build-design.md). A partition's build is the
// concurrent build executor's own (cic_build.go), with the same budget and
// failure verdict as an index on a plain table.

package executor

import "time"

// PartitionIndexAction says what the build did to give one partition an index
// attached to the parent's.
type PartitionIndexAction string

const (
	// PartitionIndexBuilt means the build created the partition's index
	// concurrently, then attached it.
	PartitionIndexBuilt PartitionIndexAction = "built"
)

// PartitionIndexResult is one partition the build gave an index. A partition
// that already had its index attached is counted in
// PartitionedIndexReport.Partitions and not listed here.
type PartitionIndexResult struct {
	// Schema and Table identify the partition.
	Schema string `json:"schema"`
	Table  string `json:"table"`
	// Index is the name of the partition's index that is now attached.
	Index string `json:"index"`
	// IndexOID is that index's catalog identity.
	IndexOID uint32 `json:"index_oid"`
	// Action says how the partition got its index.
	Action PartitionIndexAction `json:"action"`
}

// PartitionedIndexReport says what a partitioned index build did. It is
// returned only after the executor re-read the parent's index and verified it
// valid, so it means "verified valid, every partition attached".
type PartitionedIndexReport struct {
	// Schema and Index identify the parent's index.
	Schema string `json:"schema"`
	Index  string `json:"index"`
	// IndexOID is the verified index's catalog identity.
	IndexOID uint32 `json:"index_oid"`
	// ParentCreated is true when this run created the parent's index.
	ParentCreated bool `json:"parent_created"`
	// Partitions is the number of partitions in the table.
	Partitions int `json:"partitions"`
	// Worked lists the partitions this run gave an index, in order.
	Worked []PartitionIndexResult `json:"worked,omitempty"`
	// Duration is the wall-clock time of the whole build. It encodes as
	// integer nanoseconds.
	Duration time.Duration `json:"duration_ns"`
	// ServerVersion is the server_version of the server that ran the build.
	ServerVersion string `json:"server_version"`
}

// partitionedIndexRequest is the admitted statement's identity facts.
type partitionedIndexRequest struct {
	schema string
	table  string
	index  string
}
