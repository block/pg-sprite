package preflight

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// CopySwapCauseLogicalDecodingUnavailable means the server's wal_level
	// is not logical, so no replication slot can decode the table's
	// changes; on Aurora/RDS the setting behind it is
	// rds.logical_replication, a static parameter that needs a reboot.
	CopySwapCauseLogicalDecodingUnavailable CopySwapRefusalCause = "copy-and-swap-logical-decoding-unavailable"
	// CopySwapCauseSlotHeadroom means the cluster has no free replication
	// slot or no free WAL sender for the one logical-decoding connection
	// the route opens.
	CopySwapCauseSlotHeadroom CopySwapRefusalCause = "copy-and-swap-slot-headroom"
	// CopySwapCauseDiskHeadroom means the volume's free space is below the
	// headroom the shadow copy needs, or was not measured at all.
	CopySwapCauseDiskHeadroom CopySwapRefusalCause = "copy-and-swap-disk-headroom"
)

// copySwapDiskHeadroomFactor is the multiple of the source's total size
// (heap, indexes, TOAST) the volume must have free: the shadow grows to the
// source's size, and the retained WAL, the old table kept until the
// post-commit drop, and index-build spill all need room beyond it.
const copySwapDiskHeadroomFactor = 2

// walLevelLogical is the wal_level value that enables logical decoding.
const walLevelLogical = "logical"

// CopySwapSetting is a server setting an environment refusal asks the
// operator to change. It is the machine-readable half of a refusal's
// Detail: an adapter can branch on it where prose would not do.
type CopySwapSetting string

const (
	// CopySwapSettingWALLevel is wal_level itself, which a self-managed
	// server changes directly.
	CopySwapSettingWALLevel CopySwapSetting = "wal_level"
	// CopySwapSettingRDSLogicalReplication is the parameter-group switch
	// that drives wal_level on Aurora and RDS, where wal_level cannot be
	// set directly. Both are static and take effect after a restart.
	CopySwapSettingRDSLogicalReplication CopySwapSetting = "rds.logical_replication"
	// CopySwapSettingMaxReplicationSlots is the cluster-wide slot limit.
	CopySwapSettingMaxReplicationSlots CopySwapSetting = "max_replication_slots"
	// CopySwapSettingMaxWALSenders is the cluster-wide WAL sender limit.
	CopySwapSettingMaxWALSenders CopySwapSetting = "max_wal_senders"
)

// CopySwapEnvironment states what the run needs from the cluster and what
// the caller knows about it that the server cannot report.
type CopySwapEnvironment struct {
	// LogicalDecoding is whether the run captures changes through a
	// replication slot. When true the server must have wal_level =
	// logical and a free replication slot and WAL sender; a quiesced run
	// has no such need.
	LogicalDecoding bool
	// FreeDiskBytes is the free space on the volume the database writes
	// to, as the caller measured it — PostgreSQL has no function that
	// reports it. A value that is not positive is refused as unmeasured
	// rather than treated as unlimited.
	FreeDiskBytes int64
}

// CopySwapEnvironmentError reports that the cluster or the volume cannot
// carry a copy-and-swap of the proven target. Setting names the server
// setting that admits the run once changed; Detail names the measured fact
// that decided it.
type CopySwapEnvironmentError struct {
	// Cause is the environment fact that triggered the refusal.
	Cause CopySwapRefusalCause
	// Setting is the server setting the operator must change, or the zero
	// value when no setting is involved (the volume is simply too full).
	Setting CopySwapSetting
	// Detail is the measurement behind the cause.
	Detail string
}

// Error implements the error interface.
func (e *CopySwapEnvironmentError) Error() string {
	return fmt.Sprintf("copy-and-swap refuses the environment (%s): %s", e.Cause, e.Detail)
}

// copySwapEnvironmentFacts is one snapshot of every cluster fact the
// environment check decides on, gathered in a single round trip.
type copySwapEnvironmentFacts struct {
	walLevel            string
	rdsParameterPresent bool
	maxReplicationSlots int64
	usedSlots           int64
	maxWALSenders       int64
	usedWALSenders      int64
	totalBytes          int64
}

// CheckCopySwapEnvironment verifies that the cluster and the volume can
// carry a copy-and-swap of the proven target: logical decoding is enabled
// and has a free slot and WAL sender when the run decodes WAL, and the
// caller-measured free disk covers the shadow copy. It runs after
// CheckCopySwapShape and before the first write; a refusal is a
// *CopySwapEnvironmentError.
func CheckCopySwapEnvironment(ctx context.Context, pool *pgxpool.Pool, target CopySwapTarget, env CopySwapEnvironment) error {
	if target.Table() == "" || target.oid == 0 {
		return fmt.Errorf("%w: zero copy-and-swap target", ErrCopySwapProofMismatch)
	}
	facts, err := gatherCopySwapEnvironmentFacts(ctx, pool, target)
	if err != nil {
		return err
	}
	// INV: ST-6 — enablement, headroom, and disk are decided before any
	// write, from live settings and a live size, never from assumptions.
	if refusal := refuseCopySwapEnvironment(facts, env); refusal != nil {
		return refusal
	}
	return nil
}

// refuseCopySwapEnvironment decides the first cause that puts the facts
// outside what the run needs, or nil when the environment is sufficient.
// Enablement is decided before capacity, and capacity before disk, so the
// refusal names the change the operator must make first.
func refuseCopySwapEnvironment(f copySwapEnvironmentFacts, env CopySwapEnvironment) *CopySwapEnvironmentError {
	if env.LogicalDecoding {
		if f.walLevel != walLevelLogical {
			return refuseLogicalDecoding(f)
		}
		if free := f.maxReplicationSlots - f.usedSlots; free < 1 {
			return &CopySwapEnvironmentError{
				Cause:   CopySwapCauseSlotHeadroom,
				Setting: CopySwapSettingMaxReplicationSlots,
				Detail:  fmt.Sprintf("%d of max_replication_slots = %d are in use; one free slot is required", f.usedSlots, f.maxReplicationSlots),
			}
		}
		if free := f.maxWALSenders - f.usedWALSenders; free < 1 {
			return &CopySwapEnvironmentError{
				Cause:   CopySwapCauseSlotHeadroom,
				Setting: CopySwapSettingMaxWALSenders,
				Detail:  fmt.Sprintf("%d of max_wal_senders = %d are in use; one free WAL sender is required", f.usedWALSenders, f.maxWALSenders),
			}
		}
	}
	if env.FreeDiskBytes <= 0 {
		return &CopySwapEnvironmentError{
			Cause:  CopySwapCauseDiskHeadroom,
			Detail: fmt.Sprintf("free disk space was not measured (%d bytes reported); the volume's free space must be passed in", env.FreeDiskBytes),
		}
	}
	required := f.totalBytes * copySwapDiskHeadroomFactor
	if env.FreeDiskBytes < required {
		return &CopySwapEnvironmentError{
			Cause:  CopySwapCauseDiskHeadroom,
			Detail: fmt.Sprintf("%d bytes are free; %d bytes (%d× the table's %d bytes of heap, indexes, and TOAST) are required", env.FreeDiskBytes, required, copySwapDiskHeadroomFactor, f.totalBytes),
		}
	}
	return nil
}

// refuseLogicalDecoding names the setting that enables logical decoding on
// this server: the RDS parameter where the server carries it, because
// wal_level cannot be set directly there; wal_level itself everywhere else,
// including other managed services that expose their own switch for it.
func refuseLogicalDecoding(f copySwapEnvironmentFacts) *CopySwapEnvironmentError {
	if f.rdsParameterPresent {
		return &CopySwapEnvironmentError{
			Cause:   CopySwapCauseLogicalDecodingUnavailable,
			Setting: CopySwapSettingRDSLogicalReplication,
			Detail:  fmt.Sprintf("wal_level is %s; set rds.logical_replication = 1 in the parameter group (a static parameter, applied by reboot) so that wal_level = logical", f.walLevel),
		}
	}
	return &CopySwapEnvironmentError{
		Cause:   CopySwapCauseLogicalDecodingUnavailable,
		Setting: CopySwapSettingWALLevel,
		Detail:  fmt.Sprintf("wal_level is %s; set wal_level = logical (a restart-only setting)", f.walLevel),
	}
}

// gatherCopySwapEnvironmentFacts snapshots the settings, the slot and WAL
// sender occupancy, and the proven relation's total size in one query.
// Occupancy counts every slot and every WAL sender on the cluster, whatever
// database owns it, because the limits are cluster-wide.
func gatherCopySwapEnvironmentFacts(ctx context.Context, pool *pgxpool.Pool, target CopySwapTarget) (copySwapEnvironmentFacts, error) {
	const q = `
		SELECT current_setting('wal_level')::text,
		       current_setting('rds.logical_replication', true) IS NOT NULL,
		       current_setting('max_replication_slots')::bigint,
		       (SELECT count(*) FROM pg_replication_slots),
		       current_setting('max_wal_senders')::bigint,
		       (SELECT count(*) FROM pg_stat_replication),
		       (SELECT pg_total_relation_size(c.oid) FROM pg_class c WHERE c.oid = $1)`
	var f copySwapEnvironmentFacts
	var totalBytes *int64
	err := pool.QueryRow(ctx, q, target.oid).Scan(
		&f.walLevel, &f.rdsParameterPresent, &f.maxReplicationSlots, &f.usedSlots, &f.maxWALSenders, &f.usedWALSenders, &totalBytes)
	if err != nil {
		return copySwapEnvironmentFacts{}, fmt.Errorf("gather copy-and-swap environment facts for %s: %w", qualifiedName(target.Schema(), target.Table()), err)
	}
	if totalBytes == nil {
		return copySwapEnvironmentFacts{}, fmt.Errorf("%w: relation OID %d no longer exists", ErrCopySwapProofMismatch, target.oid)
	}
	f.totalBytes = *totalBytes
	return f, nil
}
