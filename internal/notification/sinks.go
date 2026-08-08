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

// Kind returns the sink's configured kind or its recording default.
func (s *RecordingSink) Kind() string {
	if s.SinkKind == "" {
		return "recording"
	}
	return s.SinkKind
}

// Version returns the sink's configured version or its v1 default.
func (s *RecordingSink) Version() string {
	if s.SinkVersion == "" {
		return "v1"
	}
	return s.SinkVersion
}

// Deliver records the exact request unless the context is canceled.
func (s *RecordingSink) Deliver(ctx context.Context, request apiv1.NotificationRequest) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	s.mu.Lock()
	s.requests = append(s.requests, request)
	s.mu.Unlock()
	return "", s.Err
}

// Requests returns a snapshot of recorded requests.
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

// NewTerminalSink creates a terminal sink writing to writer.
func NewTerminalSink(writer io.Writer) (*TerminalSink, error) {
	if writer == nil {
		return nil, fmt.Errorf("notification: terminal writer is required")
	}
	return &TerminalSink{writer: writer}, nil
}

// Kind returns the terminal sink kind.
func (*TerminalSink) Kind() string { return "terminal" }

// Version returns the terminal sink contract version.
func (*TerminalSink) Version() string { return "v1" }

// Deliver writes the request's exact content to the terminal writer.
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
