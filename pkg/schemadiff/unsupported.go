package schemadiff

import "fmt"

// UnsupportedCause names why [Diff], [IntrospectEnumType], or [DiffEnumType]
// refused to converge live onto desired. Every ErrUnsupportedChange they
// return is an *UnsupportedChangeError carrying one of [UnsupportedCauses],
// so a caller classifies the refusal by its cause and never by its message.
type UnsupportedCause string

const (
	// CauseRowSecurity refuses when the desired model carries row security, which a
	// table-only diff does not manage.
	CauseRowSecurity UnsupportedCause = "row-security"
	// CausePartitionKey refuses when live and desired disagree on the partition key.
	CausePartitionKey UnsupportedCause = "partition-key"
	// CausePartitionAttachment refuses when one side is a partition and the other is not.
	CausePartitionAttachment UnsupportedCause = "partition-attachment"
	// CausePersistence refuses when one side is unlogged and the other is not.
	CausePersistence UnsupportedCause = "persistence"
	// CauseStorageParameters refuses when live and desired declare different storage
	// parameters.
	CauseStorageParameters UnsupportedCause = "storage-parameters"
	// CauseSerialDefault refuses when a column to add or alter takes a sequence-backed
	// default, whose sequence the plan cannot create.
	CauseSerialDefault UnsupportedCause = "serial-default"
	// CauseAddedColumnCollation refuses when a column to add declares an explicit
	// collation, which the plan cannot carry.
	CauseAddedColumnCollation UnsupportedCause = "added-column-collation"
	// CauseCollationChange refuses when an existing column's collation differs.
	CauseCollationChange UnsupportedCause = "collation-change"
	// CauseIdentity refuses when an existing column gains or loses identity, or a column
	// to add declares an identity the plan cannot express.
	CauseIdentity UnsupportedCause = "identity"
	// CauseIdentitySequence refuses when an identity's sequence declaration differs.
	CauseIdentitySequence UnsupportedCause = "identity-sequence"
	// CauseGenerated refuses when an existing column becomes or stops being generated,
	// changes between stored and virtual, or changes type while generated.
	CauseGenerated UnsupportedCause = "generated"
	// CauseEnumTypeMissing refuses when the enum type a desired enum type file
	// declares does not exist in the schema.
	CauseEnumTypeMissing UnsupportedCause = "enum-type-missing"
	// CauseEnumTypeKind refuses when the name a desired enum type file
	// declares holds a type that is not an enum.
	CauseEnumTypeKind UnsupportedCause = "enum-type-kind"
	// CauseEnumValueRemoved refuses when the live enum type carries a label
	// the desired file no longer lists, removed or renamed.
	CauseEnumValueRemoved UnsupportedCause = "enum-value-removed"
	// CauseEnumValueReordered refuses when the desired file lists the live
	// enum type's labels in another order.
	CauseEnumValueReordered UnsupportedCause = "enum-value-reordered"
)

// UnsupportedCauses is the closed set of causes [Diff], [IntrospectEnumType],
// and [DiffEnumType] refuse with.
func UnsupportedCauses() []UnsupportedCause {
	return []UnsupportedCause{
		CauseRowSecurity,
		CausePartitionKey,
		CausePartitionAttachment,
		CausePersistence,
		CauseStorageParameters,
		CauseSerialDefault,
		CauseAddedColumnCollation,
		CauseCollationChange,
		CauseIdentity,
		CauseIdentitySequence,
		CauseGenerated,
		CauseEnumTypeMissing,
		CauseEnumTypeKind,
		CauseEnumValueRemoved,
		CauseEnumValueReordered,
	}
}

// UnsupportedChangeError is the change [Diff] refused, with its cause. It
// matches ErrUnsupportedChange through errors.Is.
type UnsupportedChangeError struct {
	Cause UnsupportedCause
	err   error
}

func (e *UnsupportedChangeError) Error() string {
	if e.err == nil {
		return ErrUnsupportedChange.Error() + ": " + string(e.Cause)
	}
	return e.err.Error()
}

func (e *UnsupportedChangeError) Unwrap() error {
	if e.err == nil {
		return ErrUnsupportedChange
	}
	return e.err
}

// unsupported returns the refusal for cause. format must wrap
// ErrUnsupportedChange with %w, so the message reads as it always has and the
// sentinel still matches.
func unsupported(cause UnsupportedCause, format string, args ...any) error {
	return &UnsupportedChangeError{Cause: cause, err: fmt.Errorf(format, args...)}
}
