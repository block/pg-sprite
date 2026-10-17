package decode

// Column is one decoded column: its name, and the value pgoutput carried
// for it when it carried one. An absent column is the unchanged-TOAST
// marker — pgoutput omitted the value because the stored one is unchanged,
// so an applier leaves the stored value untouched (CO-8) — and it has no
// value to read: Get reports the absence instead of a value. A SQL NULL is
// a present column whose value is nil, which is why presence is read with
// the value and never inferred from it.
type Column struct {
	name    string
	value   any
	present bool
}

// PresentColumn is a column pgoutput carried a value for. A NULL is a
// present column with a nil value.
func PresentColumn(name string, value any) Column {
	return Column{name: name, value: value, present: true}
}

// AbsentColumn is a column pgoutput sent as the unchanged-TOAST marker: the
// stored value is unchanged and was not carried.
func AbsentColumn(name string) Column {
	return Column{name: name}
}

// Name is the column's name as the relation declares it.
func (c Column) Name() string { return c.name }

// Present reports whether the column carries a value; a NULL does.
func (c Column) Present() bool { return c.present }

// Get returns the column's value and whether the column carries one. An
// absent column yields nil and false, and the nil is not a value: the
// stored value is unchanged and unknown here.
func (c Column) Get() (value any, present bool) {
	if !c.present {
		return nil, false
	}
	return c.value, true
}
