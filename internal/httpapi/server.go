package httpapi

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const readHeaderTimeout = 5 * time.Second

// ServerOption configures optional HTTP server lifecycle bounds.
type ServerOption func(*http.Server) error

// WithReadTimeout bounds the time spent reading a complete request.
func WithReadTimeout(timeout time.Duration) ServerOption {
	return func(server *http.Server) error {
		if timeout <= 0 {
			return errors.New("http server read timeout must be positive")
		}
		server.ReadTimeout = timeout
		return nil
	}
}

// WithTLS serves the listener over TLS from an on-disk certificate/key pair.
// The pair is loaded eagerly so a bad path or mismatched pair fails server
// construction rather than the first connection.
func WithTLS(certFile, keyFile string) ServerOption {
	return func(server *http.Server) error {
		if certFile == "" || keyFile == "" {
			return errors.New("http server TLS requires both a certificate file and a key file")
		}
		certificate, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return fmt.Errorf("load TLS certificate: %w", err)
		}
		server.TLSConfig = &tls.Config{
			Certificates: []tls.Certificate{certificate},
			MinVersion:   tls.VersionTLS12,
		}
		return nil
	}
}

// Server owns the HTTP listener and graceful lifecycle.
type Server struct {
	address string
	http    *http.Server
	// secure records whether TLS was configured, captured at construction so
	// Scheme/Start never read http.Server.TLSConfig — the net/http package
	// mutates that field from the serve goroutine (lazy HTTP/2 ALPN setup),
	// which would race a concurrent Scheme() read.
	secure bool

	mu       sync.Mutex
	listener net.Listener
	done     chan struct{}
	errors   chan error

	handlerShutdown func()
	shutdownOnce    sync.Once
}

// NewServer constructs an unstarted server.
func NewServer(address string, handler http.Handler, errorLog *log.Logger, opts ...ServerOption) (*Server, error) {
	if address == "" {
		return nil, errors.New("http API listen address is required")
	}
	if handler == nil {
		return nil, errors.New("http API handler is required")
	}
	if errorLog == nil {
		return nil, errors.New("http API error logger is required")
	}
	httpServer := &http.Server{
		Handler:           handler,
		ErrorLog:          errorLog,
		ReadHeaderTimeout: readHeaderTimeout,
	}
	for _, opt := range opts {
		if opt == nil {
			return nil, errors.New("http server option is required")
		}
		if err := opt(httpServer); err != nil {
			return nil, fmt.Errorf("configure HTTP server: %w", err)
		}
	}
	// Fail closed off-loopback (#640, SEC-043): a non-loopback bind is refused
	// unless the transport is encrypted AND a real authenticator gates the
	// handler. Config validation enforces the same rule at load time; this
	// second gate makes an accidentally open network API structurally
	// impossible no matter how the server is wired, and there is no override.
	if !loopbackAddress(address) {
		if httpServer.TLSConfig == nil {
			return nil, fmt.Errorf(
				"refusing to serve the HTTP API on non-loopback address %s without TLS; configure api.tls or bind a loopback address (SEC-043, #640)",
				address,
			)
		}
		if !handlerAuthenticated(handler) {
			return nil, fmt.Errorf(
				"refusing to serve the HTTP API on non-loopback address %s without an authenticator; configure api.auth or bind a loopback address (SEC-043, #640)",
				address,
			)
		}
	}
	server := &Server{
		address: address,
		http:    httpServer,
		secure:  httpServer.TLSConfig != nil,
		done:    make(chan struct{}),
		errors:  make(chan error, 1),
	}
	if lifecycle, ok := handler.(interface{ shutdown() }); ok {
		server.handlerShutdown = lifecycle.shutdown
	}
	return server, nil
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
	serve := s.http.Serve
	if s.secure {
		serve = func(listener net.Listener) error { return s.http.ServeTLS(listener, "", "") }
	}
	go func() {
		defer close(s.done)
		defer close(s.errors)
		if err := serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.errors <- err
		}
	}()
	return nil
}

// Scheme is the URL scheme the server answers on.
func (s *Server) Scheme() string {
	if s.secure {
		return "https"
	}
	return "http"
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

// loopbackAddress reports whether address provably binds a loopback host.
// Anything unparsable or ambiguous (an empty/wildcard host, a hostname other
// than localhost) counts as non-loopback so the hardening gate fails closed.
func loopbackAddress(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// handlerAuthenticated reports whether the handler advertises a real (non-
// null) Authenticator. Handlers that do not advertise at all count as
// unauthenticated — fail closed.
func handlerAuthenticated(handler http.Handler) bool {
	authed, ok := handler.(interface{ authenticatedTransport() bool })
	return ok && authed.authenticatedTransport()
}

// Shutdown gracefully stops accepting requests and waits for active handlers.
func (s *Server) Shutdown(ctx context.Context) error {
	s.shutdownOnce.Do(func() {
		if s.handlerShutdown != nil {
			s.handlerShutdown()
		}
	})
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
