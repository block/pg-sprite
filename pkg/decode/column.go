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
// present column with a nil value. The stream carries the text form
// pgoutput sent, so a value it decodes is a string; the applier binds a
// value as a text parameter, so a caller building its own events passes a
// string or nil.
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

// Get returns the column's value together with whether the column carries
// one, so a reader takes the presence with the value. An absent column
// yields nil and false, and the nil is not a value: the stored value is
// unchanged and unknown here. Go still lets a caller discard the presence;
// a test in this package refuses that in production code, and the
// applier's bind refuses an absent column before any statement runs.
func (c Column) Get() (value any, present bool) {
	if !c.present {
		return nil, false
	}
	return c.value, true
}

// String renders the column as name=<absent>, name=NULL, or name=<value>,
// so a log line or a test failure tells the three apart. It never renders
// the value itself, which is row data. A Column has no exported fields, so
// it carries no JSON encoding.
func (c Column) String() string {
	switch {
	case !c.present:
		return c.name + "=<absent>"
	case c.value == nil:
		return c.name + "=NULL"
	default:
		return c.name + "=<value>"
	}
}
