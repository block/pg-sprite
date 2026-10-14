package checkpoint

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// validateShadowProof refuses a proof the jsonb column cannot hold or that
// no builder renders: the proof is a JSON document — an object — so a bare
// null, number, string, or array is not one, even though each is valid
// JSON (a null would read back as a present proof that decodes to nothing);
// and jsonb refuses a NUL character inside any string, so that is refused
// here rather than reaching the caller as a server error. An empty proof
// is valid: the run has no shadow to prove.
func validateShadowProof(proof json.RawMessage) error {
	if len(proof) == 0 {
		return nil
	}
	if !json.Valid(proof) {
		return fmt.Errorf("%w: shadow proof is not JSON", ErrInvalidCheckpoint)
	}
	doc, err := decodeJSON(proof)
	if err != nil {
		return fmt.Errorf("%w: shadow proof: %w", ErrInvalidCheckpoint, err)
	}
	if _, isObject := doc.(map[string]any); !isObject {
		return fmt.Errorf("%w: shadow proof is JSON but not an object", ErrInvalidCheckpoint)
	}
	if containsNUL(doc) {
		return fmt.Errorf("%w: shadow proof contains a NUL character, which jsonb cannot hold", ErrInvalidCheckpoint)
	}
	return nil
}

// decodeJSON decodes one JSON value into Go's generic shape, keeping
// numbers as their literal text so no precision is lost.
func decodeJSON(text []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(text))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	return doc, nil
}

// containsNUL reports whether any string in the decoded document — a key
// or a value, at any depth — holds a NUL character.
func containsNUL(doc any) bool {
	switch v := doc.(type) {
	case string:
		return strings.ContainsRune(v, 0)
	case map[string]any:
		for key, value := range v {
			if strings.ContainsRune(key, 0) || containsNUL(value) {
				return true
			}
		}
	case []any:
		return slices.ContainsFunc(v, containsNUL)
	}
	return false
}

// shadowProofColumn maps the proof onto the nullable jsonb column: NULL
// when the run has none. pgx writes a nil json.RawMessage as NULL and
// anything else as the JSON text it holds, so an empty slice must become
// nil rather than reach the server as an empty document.
func shadowProofColumn(proof json.RawMessage) json.RawMessage {
	if len(proof) == 0 {
		return nil
	}
	return proof
}

// replacesProof reports whether a Save of cp would put a different proof
// on a row that already holds one — the one thing the proof column refuses
// after it is first written. A cp without a proof, or a row without one,
// replaces nothing. An error means one side is not a JSON document, which
// the row cannot be (jsonb) and cp cannot be (validate), so it is reported
// as the invariant violation it is.
func replacesProof(stored row, cp Checkpoint) (bool, error) {
	// INV: ST-2
	if len(cp.ShadowProof) == 0 || len(stored.shadowProof) == 0 {
		return false, nil
	}
	same, err := sameJSON(stored.shadowProof, cp.ShadowProof)
	if err != nil {
		return false, fmt.Errorf("%w: shadow proof could not be compared with the row's: %w", ErrInvariantViolation, err)
	}
	return !same, nil
}

// sameJSON reports whether two JSON documents are the same value, whatever
// their spacing and key order: the server stores jsonb by value and renders
// it its own way, so the proof a row holds and the proof a Save carries
// can only be compared this way. Either side failing to decode is an
// error, not a difference.
func sameJSON(a, b json.RawMessage) (bool, error) {
	docA, err := decodeJSON(a)
	if err != nil {
		return false, err
	}
	docB, err := decodeJSON(b)
	if err != nil {
		return false, err
	}
	return sameValue(docA, docB), nil
}

// sameValue compares two decoded JSON values structurally.
func sameValue(a, b any) bool {
	switch va := a.(type) {
	case map[string]any:
		vb, ok := b.(map[string]any)
		if !ok || len(va) != len(vb) {
			return false
		}
		for key, value := range va {
			other, present := vb[key]
			if !present || !sameValue(value, other) {
				return false
			}
		}
		return true
	case []any:
		vb, ok := b.([]any)
		if !ok || len(va) != len(vb) {
			return false
		}
		for i := range va {
			if !sameValue(va[i], vb[i]) {
				return false
			}
		}
		return true
	default:
		return a == b
	}
}
