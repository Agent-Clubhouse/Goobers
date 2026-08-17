// Package speechnotify provides test doubles for speech notifications.
package speechnotify

import (
	"context"
	"sync"

	"github.com/goobers/goobers/internal/speechnotify"
)

// FakeSynthesizer is a configurable synthesizer that records utterances.
type FakeSynthesizer struct {
	Report       speechnotify.Preflight
	PreflightErr error
	Err          error

	mu         sync.Mutex
	utterances []string
}

// Name returns the fake synthesizer's engine name.
func (*FakeSynthesizer) Name() string { return "fake" }

// Preflight returns the configured report and error.
func (f *FakeSynthesizer) Preflight(context.Context, speechnotify.Config) (speechnotify.Preflight, error) {
	report := f.Report
	if report.Engine == "" {
		report = speechnotify.Preflight{
			Engine:            "fake",
			Executable:        "in-process",
			Voice:             "fake",
			Language:          "und",
			Rate:              speechnotify.DefaultRate,
			AudioPrerequisite: "none",
			AudioAvailable:    true,
		}
	}
	return report, f.PreflightErr
}

// Synthesize records text unless the context is canceled.
func (f *FakeSynthesizer) Synthesize(ctx context.Context, _ speechnotify.Config, text string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	f.utterances = append(f.utterances, text)
	f.mu.Unlock()
	return f.Err
}

// Utterances returns a copy of the recorded utterances.
func (f *FakeSynthesizer) Utterances() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.utterances...)
}
