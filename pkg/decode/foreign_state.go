package decode

import (
	"errors"
	"fmt"
)

// ErrForeignDecodingState is the sentinel every refusal of foreign state
// unwraps to: the publication or the slot of the derived name turned out
// not to be this route's. Neither is ever adopted or dropped; the operator
// resolves it by renaming or dropping the foreign object. The refusal is
// deliberately total — a foreign slot leaves the publication of the name in
// place too — and a caller must not answer it with DropSlot, which would
// wait out and drop whatever the other owner is still streaming from.
var ErrForeignDecodingState = errors.New("logical-decoding state of the derived name is not this route's")

// ForeignObject names which logical-decoding object of the derived name a
// ForeignStateError is about.
type ForeignObject string

const (
	// ForeignObjectSlot is a replication slot of the derived name that
	// belongs to another database or is physical.
	ForeignObjectSlot ForeignObject = "slot"
	// ForeignObjectPublication is a publication of the derived name that is
	// not the one CreateSlot would have created for the target.
	ForeignObjectPublication ForeignObject = "publication"
)

// ForeignStateError is the refusal CreateSlot and DropSlot return for
// foreign state, so a caller can route on which object was foreign and
// report why without reading message text. It unwraps to
// ErrForeignDecodingState.
type ForeignStateError struct {
	Object ForeignObject
	// Name is the derived name the object wears.
	Name string
	// Detail says what made the object foreign: the database that owns the
	// slot, or what the publication publishes that the route's would not.
	Detail string
}

func (e *ForeignStateError) Error() string {
	return fmt.Sprintf("%s %s is not this route's: %s", e.Object, e.Name, e.Detail)
}

// Unwrap makes every ForeignStateError match ErrForeignDecodingState.
func (e *ForeignStateError) Unwrap() error { return ErrForeignDecodingState }

// foreignSlot is the refusal for a slot of the derived name the pool's
// database does not own.
func foreignSlot(name string) error {
	return &ForeignStateError{Object: ForeignObjectSlot, Name: name,
		Detail: "it belongs to another database or is a physical slot"}
}

// foreignPublication is the refusal for a publication of the derived name
// that is not the route's.
func foreignPublication(name, detail string) error {
	return &ForeignStateError{Object: ForeignObjectPublication, Name: name, Detail: detail}
}
