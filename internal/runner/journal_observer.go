package runner

import (
	"context"

	"github.com/goobers/goobers/internal/journal"
)

func (r *Runner) journalObserver(ctx context.Context) journal.Option {
	if r.cfg.JournalAdvancedContext != nil {
		return journal.WithAsyncAppendObserver(ctx, r.cfg.JournalAdvancedContext)
	}
	return journal.WithAppendObserver(r.cfg.JournalAdvanced)
}
