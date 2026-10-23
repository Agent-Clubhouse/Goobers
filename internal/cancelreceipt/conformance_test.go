package cancelreceipt_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/cancelreceipt"
	"github.com/goobers/goobers/test/testsupport/conformance/receipts"
)

// clocked binds the local store to the host clock, the reference backend's
// only time source.
type clocked struct{ store *cancelreceipt.Store }

func (c clocked) Begin(ctx context.Context, key, actor string, payload []byte) (cancelreceipt.Receipt, bool, error) {
	return c.store.Begin(ctx, key, actor, payload, time.Now())
}

func (c clocked) Finish(ctx context.Context, key string, result []byte) error {
	return c.store.Finish(ctx, key, result, time.Now())
}

func TestReceiptsConformance(t *testing.T) {
	receipts.Run(t, func(t *testing.T) receipts.Backend {
		path := filepath.Join(t.TempDir(), "receipts.db")
		return receipts.Backend{Open: func(t *testing.T) cancelreceipt.Receipts {
			store, err := cancelreceipt.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			return clocked{store: store}
		}}
	})
}
