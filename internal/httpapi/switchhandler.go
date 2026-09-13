package httpapi

import (
	"errors"
	"net/http"
	"sync"
)

// SwitchHandler lets a server bind with a minimal startup surface and replace
// it with the full API without closing or rebinding the listener.
type SwitchHandler struct {
	mu            sync.RWMutex
	handler       http.Handler
	authenticated bool
}

// NewSwitchHandler constructs a handler whose authentication posture is fixed
// for the server lifetime even while its request handler is replaced.
func NewSwitchHandler(initial http.Handler, authenticated bool) (*SwitchHandler, error) {
	if initial == nil {
		return nil, errors.New("initial HTTP handler is required")
	}
	return &SwitchHandler{handler: initial, authenticated: authenticated}, nil
}

func (s *SwitchHandler) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	s.mu.RLock()
	handler := s.handler
	s.mu.RUnlock()
	handler.ServeHTTP(response, request)
}

// Set replaces the handler used for subsequent requests.
func (s *SwitchHandler) Set(handler http.Handler) error {
	if handler == nil {
		return errors.New("replacement HTTP handler is required")
	}
	s.mu.Lock()
	s.handler = handler
	s.mu.Unlock()
	return nil
}

func (s *SwitchHandler) authenticatedTransport() bool {
	return s.authenticated
}

func (s *SwitchHandler) shutdown() {
	s.mu.RLock()
	handler := s.handler
	s.mu.RUnlock()
	if lifecycle, ok := handler.(interface{ shutdown() }); ok {
		lifecycle.shutdown()
	}
}
