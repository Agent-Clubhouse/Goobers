package terminalreport_test

import (
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/terminalreport"
	"github.com/goobers/goobers/test/testsupport/conformance/outbox"
)

func TestOutboxConformance(t *testing.T) {
	outbox.Run(t, func(t *testing.T) outbox.Backend {
		dir := t.TempDir()
		var mu sync.Mutex
		offset := time.Duration(0)
		now := func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			return time.Now().Add(offset)
		}
		return outbox.Backend{
			Open: func(*testing.T) terminalreport.Outbox { return terminalreport.Queue{Dir: dir, Now: now} },
			Expire: func(*testing.T) {
				mu.Lock()
				defer mu.Unlock()
				offset += 2 * time.Hour
			},
		}
	})
}
