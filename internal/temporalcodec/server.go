package temporalcodec

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/oidcauth"
)

// NewServer constructs the remote codec endpoint. OIDC and listener TLS are
// mandatory, including on loopback; a local daemon's anonymous mode is never
// inherited. No listener or outbound connection is opened by construction.
func NewServer(cfg *instance.Config, address, certFile, keyFile string, origins []string) (*http.Server, error) {
	if cfg == nil || cfg.API.Auth == nil || cfg.API.Auth.OIDC == nil {
		return nil, fmt.Errorf("codec server requires api.auth.oidc")
	}
	if certFile == "" || keyFile == "" {
		return nil, fmt.Errorf("codec server requires TLS certificate and key")
	}
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("codec server: load TLS certificate and key: %w", err)
	}
	codec, err := FromConfig(cfg)
	if err != nil {
		return nil, err
	}
	auth := cfg.API.Auth.OIDC
	handler, err := NewHTTPHandler(codec, oidcauth.Config{
		Issuer: auth.Issuer, Audience: auth.Audience, RolesClaim: auth.RolesClaimName(),
		Roles: oidcauth.RoleMapping{View: auth.Roles.View, Operate: auth.Roles.Operate, Admin: auth.Roles.Admin},
	}, origins)
	if err != nil {
		return nil, err
	}
	return &http.Server{
		Addr: address, Handler: handler,
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair}},
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 20 * time.Second,
		WriteTimeout: 45 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10,
	}, nil
}

// Serve owns listener and joins the serving goroutine before returning. Cancel
// stops key operations via request contexts and allows at most five seconds to
// drain HTTP connections before forcing closure.
func Serve(ctx context.Context, server *http.Server, listener net.Listener) error {
	if server == nil || server.TLSConfig == nil || len(server.TLSConfig.Certificates) == 0 {
		_ = listener.Close()
		return fmt.Errorf("codec server requires TLS")
	}
	defer func() { _ = listener.Close() }()
	requestCtx, cancelRequests := context.WithCancel(ctx)
	defer cancelRequests()
	server.BaseContext = func(net.Listener) context.Context { return requestCtx }
	done := make(chan error, 1)
	go func() { done <- server.ServeTLS(listener, "", "") }()
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		cancelRequests()
		drain, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := server.Shutdown(drain)
		if err != nil {
			_ = server.Close()
		}
		serveErr := <-done
		if err != nil {
			return err
		}
		if errors.Is(serveErr, http.ErrServerClosed) {
			return nil
		}
		return serveErr
	}
}
