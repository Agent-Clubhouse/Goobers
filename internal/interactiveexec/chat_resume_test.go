//go:build !windows

package interactiveexec

import (
	"context"
	"errors"
	"testing"

	"github.com/goobers/goobers/internal/chat"
	"github.com/goobers/goobers/internal/chat/chattest"
	"github.com/goobers/goobers/internal/harness"
)

type resumeNoteDriver struct {
	chattest.FakeDriver
	fallback chat.ResumeFallback
}

func (d *resumeNoteDriver) Start(ctx context.Context, options chat.StartOptions) (chat.Session, error) {
	session, err := d.FakeDriver.Start(ctx, options)
	return resumeNoteSession{Session: session, fallback: d.fallback}, err
}

type resumeNoteSession struct {
	chat.Session
	fallback chat.ResumeFallback
}

func (s resumeNoteSession) ResumeFallback() chat.ResumeFallback { return s.fallback }

func TestChatResumeFallbackNote(t *testing.T) {
	for _, tc := range []struct {
		name     string
		fallback chat.ResumeFallback
		message  string
	}{
		{"missing", chat.ResumeNotFound, "Previous session was not found; started a fresh session."},
		{"stalled", chat.ResumeStalled, "Resuming the previous session timed out; started a fresh session."},
		{"no fallback", "", ""},
		{"unknown omitted", "untrusted private diagnostic", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			executor, request := bridgeExecutor(t)
			driver := &resumeNoteDriver{fallback: tc.fallback}
			executor.chatRegistry = func(string, harness.RunRequest) *chat.Registry {
				registry := chat.NewRegistry()
				if err := registry.Register("copilot", driver); err != nil {
					t.Fatal(err)
				}
				return registry
			}
			request.Harness = "copilot"
			stop := errors.New("note observed")
			var note map[string]any
			_, _, err := executor.driveChat(context.Background(), context.Background(), request,
				harness.RunRequest{Workspace: t.TempDir()}, nil, func(kind string, data map[string]any) error {
					if kind == "execution.note" && data["driverAttempts"] != nil {
						note = data
						return stop
					}
					return nil
				})
			if !errors.Is(err, stop) || note == nil {
				t.Fatalf("startup note was not emitted: %v", err)
			}
			attempts, ok := note["driverAttempts"].([]chat.Attempt)
			if !ok || len(attempts) != 1 || attempts[0].Driver != "fake" {
				t.Fatalf("missing startup attempts: %#v", note)
			}
			if tc.message == "" {
				if len(note) != 1 {
					t.Fatalf("unexpected resume diagnostics: %#v", note)
				}
			} else if len(note) != 3 || note["message"] != tc.message || note["resumeFallback"] != tc.fallback {
				t.Fatalf("unexpected bounded resume note: %#v", note)
			}
		})
	}
}
