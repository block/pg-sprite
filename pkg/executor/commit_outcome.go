package executor

import "fmt"

// CommitOutcomeUnknownError means a bounded attempt's COMMIT did not return
// success, and the client cannot establish whether the server applied it: the
// connection may have dropped before the reply, the server may have failed
// over, or the caller's context may have ended while the COMMIT was in
// flight. The change may or may not be in the catalog, so a caller must
// inspect the catalog before retrying — never read it as the rollback an
// attempt that failed before COMMIT gets. Err keeps the cause for errors.Is
// and errors.As.
type CommitOutcomeUnknownError struct{ Err error }

func (e *CommitOutcomeUnknownError) Error() string {
	return fmt.Sprintf("commit outcome is unknown until the catalog is inspected: %v", e.Err)
}

func (e *CommitOutcomeUnknownError) Unwrap() error { return e.Err }
