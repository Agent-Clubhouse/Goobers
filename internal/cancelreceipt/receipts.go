package cancelreceipt

import "context"

// Receipts is the storage-independent cancel-receipt contract. The local
// SQLite Store (with a clock bound) and the cloud PostgreSQL store both satisfy
// it; test/testsupport/conformance/receipts verifies they agree.
//
// Begin binds key to the actor and exact payload; only fresh=true authorizes
// execution, a matching retry returns the stored receipt, and a mismatch is
// ErrConflict. Finish records the outcome exactly once; any later completion,
// including an identical one, is ErrTransition.
type Receipts interface {
	Begin(ctx context.Context, key, actor string, payload []byte) (receipt Receipt, fresh bool, err error)
	Finish(ctx context.Context, key string, result []byte) error
}
