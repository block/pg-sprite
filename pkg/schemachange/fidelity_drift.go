package schemachange

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

// fidelityDrift names the facts of have that differ from want, in snapshot
// field order, so a refusal can say what moved rather than that something
// did. The facts are the snapshot's fields, named by their JSON tags, so a
// field added to the snapshot is compared without this function changing.
// Each fact is compared as its canonical JSON, the same encoding the
// checkpoint stores, so the two paths agree on what "equal" means.
func fidelityDrift(have, want FidelitySnapshot) ([]string, error) {
	haveValue, wantValue := reflect.ValueOf(have), reflect.ValueOf(want)
	var drifted []string
	for _, field := range reflect.VisibleFields(haveValue.Type()) {
		name := fidelityFactName(field)
		same, err := jsonEqual(haveValue.FieldByIndex(field.Index).Interface(), wantValue.FieldByIndex(field.Index).Interface())
		if err != nil {
			return nil, fmt.Errorf("compare %s: %w", name, err)
		}
		if !same {
			drifted = append(drifted, name)
		}
	}
	return drifted, nil
}

// fidelityFactName is the snapshot field's JSON name, which is the name the
// checkpoint and the refusal detail both use for the fact.
func fidelityFactName(field reflect.StructField) string {
	name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
	return name
}

// jsonEqual reports whether two values share one JSON encoding. A nil
// slice and an empty one encode differently; both sides are read by the
// same catalog reader, which yields the same shape for the same catalog.
func jsonEqual(a, b any) (bool, error) {
	encodedA, err := json.Marshal(a)
	if err != nil {
		return false, err
	}
	encodedB, err := json.Marshal(b)
	if err != nil {
		return false, err
	}
	return bytes.Equal(encodedA, encodedB), nil
}
