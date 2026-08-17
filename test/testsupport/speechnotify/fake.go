package speechnotify

import (
	"context"
	"sync"

	"github.com/goobers/goobers/internal/speechnotify"
)

type FakeSynthesizer struct {
	Report       speechnotify.Preflight
	PreflightErr error
	Err          error

	mu         sync.Mutex
	utterances []string
}

func (*FakeSynthesizer) Name() string { return "fake" }

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

func (f *FakeSynthesizer) Synthesize(ctx context.Context, _ speechnotify.Config, text string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	f.utterances = append(f.utterances, text)
	f.mu.Unlock()
	return f.Err
}

func (f *FakeSynthesizer) Utterances() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.utterances...)
}
