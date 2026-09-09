package claimsclient

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// ExecutionObserver exposes no claim mutation operations.
type ExecutionObserver struct{ client *HTTP }

// NewExecutionObserver preserves the explicitly local no-auth daemon posture.
// Anonymous observation requires a literal loopback address, never DNS, and
// refuses redirects. Normal remote observation still requires a bearer.
func NewExecutionObserver(cfg HTTPConfig) (*ExecutionObserver, error) {
	anonymous := strings.TrimSpace(cfg.Token) == ""
	if anonymous {
		address, err := url.Parse(cfg.BaseURL)
		if err != nil {
			return nil, err
		}
		ip := net.ParseIP(address.Hostname())
		if ip == nil || !ip.IsLoopback() || (address.Scheme != "http" && address.Scheme != "https") || address.User != nil || address.RawQuery != "" || address.Fragment != "" {
			return nil, fmt.Errorf("anonymous execution observation requires a literal loopback daemon address")
		}
		client := http.Client{Timeout: DefaultHTTPTimeout}
		if cfg.Client != nil {
			client = *cfg.Client
		}
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		cfg.Client = &client
	}
	client, err := newHTTP(cfg, anonymous)
	if err != nil {
		return nil, err
	}
	return &ExecutionObserver{client: client}, nil
}

// ExecutionSnapshot reads the owning run's pinned policy and execution leases.
func (o *ExecutionObserver) ExecutionSnapshot(ctx context.Context) (string, Listing, error) {
	return o.client.ExecutionSnapshot(ctx)
}
