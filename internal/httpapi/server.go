package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"
	"time"
)

const readHeaderTimeout = 5 * time.Second

// Server owns the HTTP listener and graceful lifecycle.
type Server struct {
	address string
	http    *http.Server

	mu       sync.Mutex
	listener net.Listener
	done     chan struct{}
	errors   chan error
}

// NewServer constructs an unstarted server.
func NewServer(address string, handler http.Handler, errorLog *log.Logger) (*Server, error) {
	if address == "" {
		return nil, errors.New("http API listen address is required")
	}
	if handler == nil {
		return nil, errors.New("http API handler is required")
	}
	if errorLog == nil {
		return nil, errors.New("http API error logger is required")
	}
	return &Server{
		address: address,
		http: &http.Server{
			Handler:           handler,
			ErrorLog:          errorLog,
			ReadHeaderTimeout: readHeaderTimeout,
		},
		done:   make(chan struct{}),
		errors: make(chan error, 1),
	}, nil
}

// Start binds synchronously so listener failures are reported during daemon
// startup, then serves in the background.
func (s *Server) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener != nil {
		return errors.New("http API server already started")
	}
	listener, err := net.Listen("tcp", s.address)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.address, err)
	}
	s.listener = listener
	go func() {
		defer close(s.done)
		defer close(s.errors)
		if err := s.http.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.errors <- err
		}
	}()
	return nil
}

// Address returns the bound listener address after Start.
func (s *Server) Address() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener == nil {
		return s.address
	}
	return s.listener.Addr().String()
}

// Errors reports an unexpected serving failure.
func (s *Server) Errors() <-chan error { return s.errors }

// Shutdown gracefully stops accepting requests and waits for active handlers.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	started := s.listener != nil
	s.mu.Unlock()
	if !started {
		return nil
	}
	if err := s.http.Shutdown(ctx); err != nil {
		return fmt.Errorf("shut down HTTP API: %w", err)
	}
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("wait for HTTP API shutdown: %w", ctx.Err())
	}
}
