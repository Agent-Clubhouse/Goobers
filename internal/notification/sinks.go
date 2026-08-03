package notification

import (
	"context"
	"fmt"
	"io"
	"sync"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// RecordingSink is a hermetic sink that retains exact requests.
type RecordingSink struct {
	SinkKind    string
	SinkVersion string
	Err         error

	mu       sync.Mutex
	requests []apiv1.NotificationRequest
}

func (s *RecordingSink) Kind() string {
	if s.SinkKind == "" {
		return "recording"
	}
	return s.SinkKind
}

func (s *RecordingSink) Version() string {
	if s.SinkVersion == "" {
		return "v1"
	}
	return s.SinkVersion
}

func (s *RecordingSink) Deliver(ctx context.Context, request apiv1.NotificationRequest) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	s.mu.Lock()
	s.requests = append(s.requests, request)
	s.mu.Unlock()
	return "", s.Err
}

func (s *RecordingSink) Requests() []apiv1.NotificationRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]apiv1.NotificationRequest(nil), s.requests...)
}

// TerminalSink writes exact title, body, and optional speech text as separate
// lines. It owns no credentials or provider configuration.
type TerminalSink struct {
	mu     sync.Mutex
	writer io.Writer
}

func NewTerminalSink(writer io.Writer) (*TerminalSink, error) {
	if writer == nil {
		return nil, fmt.Errorf("notification: terminal writer is required")
	}
	return &TerminalSink{writer: writer}, nil
}

func (*TerminalSink) Kind() string    { return "terminal" }
func (*TerminalSink) Version() string { return "v1" }

func (s *TerminalSink) Deliver(ctx context.Context, request apiv1.NotificationRequest) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := fmt.Fprintln(s.writer, request.Title); err != nil {
		return "", fmt.Errorf("terminal title: %w", err)
	}
	if _, err := fmt.Fprintln(s.writer, request.Body); err != nil {
		return "", fmt.Errorf("terminal body: %w", err)
	}
	if request.SpeechText != "" {
		if _, err := fmt.Fprintln(s.writer, request.SpeechText); err != nil {
			return "", fmt.Errorf("terminal speech text: %w", err)
		}
	}
	return "", nil
}
