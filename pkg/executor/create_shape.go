// This file is the create path's shape-refusal vocabulary: every reason the
// create path refuses a desired statement by its shape maps to exactly one
// flat kebab-case CreateShapeCause, the same treatment code.go gives
// executor outcomes. A plan or verdict consumer branches on the cause —
// never on error prose — and each cause owns its one human sentence, so the
// sentinel errors, the typed refusal, and every renderer read the same text.

package executor

import (
	"errors"
	"fmt"
)

// CreateShapeCause is the stable identity of why the create path refuses a
// desired statement's shape. Automation branches on it, never on error text.
// Existing values never change meaning; new refusals add new values.
type CreateShapeCause string

// The causes for which the create path can refuse a desired statement.
const (
	// CreateShapePartitionOf refuses attaching a partition because it locks
	// the partitioned parent, which the absence proof does not cover.
	CreateShapePartitionOf CreateShapeCause = "partition-of"
	// CreateShapeInherits refuses binding to an existing parent because the
	// absence proof does not cover it.
	CreateShapeInherits CreateShapeCause = "inherits"
	// CreateShapeLike refuses reading an existing source table because the
	// absence proof does not cover it.
	CreateShapeLike CreateShapeCause = "like"
	// CreateShapeOfType refuses binding to an existing composite type because
	// the absence proof does not cover it.
	CreateShapeOfType CreateShapeCause = "of-type"
	// CreateShapeIfNotExists refuses a name-only no-op because it cannot prove
	// the existing relation has the requested shape or is valid.
	CreateShapeIfNotExists CreateShapeCause = "if-not-exists"
	// CreateShapeConcurrently refuses a concurrent build because a table born
	// this run has no traffic to protect and a plain build cannot leave an
	// invalid index behind a failure.
	CreateShapeConcurrently CreateShapeCause = "concurrently"
	// CreateShapeDuplicateName refuses a desired set that claims the same
	// relation name twice before any statement runs.
	CreateShapeDuplicateName CreateShapeCause = "duplicate-name"
	// CreateShapeMultipleOperations refuses a statement when the statement and
	// operation parse boundaries disagree about its operation count.
	CreateShapeMultipleOperations CreateShapeCause = "multiple-operations"
	// CreateShapeUnsupportedKind refuses a statement kind outside the plain
	// CREATE TABLE and CREATE INDEX shapes the create path can run.
	CreateShapeUnsupportedKind CreateShapeCause = "unsupported-kind"
)

// CreateShapeCauses returns the closed set of create-shape refusal causes.
func CreateShapeCauses() []CreateShapeCause {
	return []CreateShapeCause{
		CreateShapePartitionOf,
		CreateShapeInherits,
		CreateShapeLike,
		CreateShapeOfType,
		CreateShapeIfNotExists,
		CreateShapeConcurrently,
		CreateShapeDuplicateName,
		CreateShapeMultipleOperations,
		CreateShapeUnsupportedKind,
	}
}

// Description returns the human-facing sentence for a create-shape cause.
func (c CreateShapeCause) Description() string {
	switch c {
	case CreateShapePartitionOf:
		return "PARTITION OF attaches a partition to a parent the absence proof does not cover, locking that parent"
	case CreateShapeInherits:
		return "INHERITS binds to an existing parent the absence proof does not cover"
	case CreateShapeLike:
		return "LIKE reads an existing source table the absence proof does not cover"
	case CreateShapeOfType:
		return "OF binds to an existing composite type the absence proof does not cover"
	case CreateShapeIfNotExists:
		return "IF NOT EXISTS is a name-only no-op that cannot prove the existing relation is the requested one, or even valid"
	case CreateShapeConcurrently:
		return "a concurrent build is refused on a table born this run"
	case CreateShapeDuplicateName:
		return "desired set claims the same relation name twice"
	case CreateShapeMultipleOperations:
		return "statement carries multiple operations"
	case CreateShapeUnsupportedKind:
		return "statement kind is not supported by the create path"
	default:
		return "unknown create-shape refusal"
	}
}

// CreateShapeError identifies a create-shape refusal. Name is populated only
// when Cause is CreateShapeDuplicateName.
type CreateShapeError struct {
	Cause CreateShapeCause
	Name  string
}

// Error renders the create-shape refusal for a human reader.
func (e *CreateShapeError) Error() string {
	if e.Cause == CreateShapeDuplicateName && e.Name != "" {
		return fmt.Sprintf("%s: %q", e.Cause.Description(), e.Name)
	}
	return e.Cause.Description()
}

// Unwrap exposes the existing sentinel boundary for errors.Is callers.
func (e *CreateShapeError) Unwrap() error {
	switch e.Cause {
	case CreateShapePartitionOf:
		return ErrPartitionOfUnsupported
	case CreateShapeIfNotExists:
		return ErrIfNotExistsUnsupported
	case CreateShapeDuplicateName:
		return ErrDuplicateCreateName
	case CreateShapeInherits, CreateShapeLike, CreateShapeOfType, CreateShapeConcurrently,
		CreateShapeMultipleOperations, CreateShapeUnsupportedKind:
		return ErrUnsupportedCreateStep
	default:
		return nil
	}
}

// CreateShapeCauseOf returns the create-shape cause carried by err, or the
// empty cause when err is nil or carries no create-shape refusal. Wrappers,
// including a *SequenceStepError naming the failed step, are read through.
func CreateShapeCauseOf(err error) CreateShapeCause {
	var shapeErr *CreateShapeError
	if errors.As(err, &shapeErr) {
		return shapeErr.Cause
	}
	return ""
}
