package schemadiff

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrStaleRowSecurityReview means the captured definitions no longer match a review.
var ErrStaleRowSecurityReview = errors.New("row security review is stale")

// ErrInvalidRowSecurityFingerprint means no supported review identity was supplied.
var ErrInvalidRowSecurityFingerprint = errors.New("invalid row security review fingerprint")

// RowSecurityReviewStale carries the reviewed and freshly computed identities.
// Neither identity authorizes execution.
type RowSecurityReviewStale struct {
	Expected string
	Actual   string
}

// Error describes the mismatch without treating it as an execution failure.
func (e *RowSecurityReviewStale) Error() string {
	return "row security review is stale; review the current definitions again"
}

// Unwrap identifies stale reviews independently of human-facing wording.
func (e *RowSecurityReviewStale) Unwrap() error { return ErrStaleRowSecurityReview }

const rowSecurityFingerprintPrefix = "rls-review-v1:"

// ValidateRowSecurityFingerprint rejects empty, malformed, or unknown identities.
func ValidateRowSecurityFingerprint(value string) error {
	if len(value) != len(rowSecurityFingerprintPrefix)+sha256.Size*2 {
		return ErrInvalidRowSecurityFingerprint
	}
	if value[:len(rowSecurityFingerprintPrefix)] != rowSecurityFingerprintPrefix {
		return ErrInvalidRowSecurityFingerprint
	}
	digest, err := hex.DecodeString(value[len(rowSecurityFingerprintPrefix):])
	if err != nil {
		return ErrInvalidRowSecurityFingerprint
	}
	if hex.EncodeToString(digest) != value[len(rowSecurityFingerprintPrefix):] {
		return ErrInvalidRowSecurityFingerprint
	}
	return nil
}

// CheckFingerprint compares this captured review with a previously saved identity.
// Callers must obtain a fresh review before checking. A match proves neither
// effective access nor database identity, and is not an execution permission.
func (r RowSecurityReview) CheckFingerprint(expected string) error {
	if err := ValidateRowSecurityFingerprint(expected); err != nil {
		return err
	}
	if err := ValidateRowSecurityFingerprint(r.Fingerprint); err != nil {
		return err
	}
	if r.Fingerprint != expected {
		return &RowSecurityReviewStale{Expected: expected, Actual: r.Fingerprint}
	}
	return nil
}

// Hash complete canonical catalog models, not only deltas. Slice order and null
// values are preserved: unexpected representation changes conservatively require
// a new review. Changing this encoding requires a new fingerprint prefix.
func rowSecurityFingerprint(schema string, live, desired Model) (string, error) {
	payload := struct {
		Schema  string
		Live    Model
		Desired Model
	}{schema, live, desired}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode row security review: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return rowSecurityFingerprintPrefix + hex.EncodeToString(digest[:]), nil
}
