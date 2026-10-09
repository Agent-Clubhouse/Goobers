// Package planehttp provides the shared HTTP mechanics used by daemon-plane
// clients. Policy and public errors remain owned by each client package.
package planehttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Config configures a daemon-plane client.
type Config struct {
	BaseURL      string
	Token        string
	Client       *http.Client
	Timeout      time.Duration
	AllowNoToken bool
	BaseURLError error
	TokenError   error
	// Retry tunes the transient-overload retry applied by DoRetrying and
	// DoJSONRetrying. The zero value selects the defaults.
	Retry RetryConfig
}

// Client builds authenticated requests against one daemon-plane base URL.
type Client struct {
	baseURL string
	token   string
	client  *http.Client
	retry   RetryConfig
}

// RequestError identifies which request phase failed.
type RequestError struct {
	Op       string
	Endpoint string
	Err      error
}

func (e *RequestError) Error() string { return e.Err.Error() }
func (e *RequestError) Unwrap() error { return e.Err }

// New validates and normalizes config and supplies a bounded default client.
func New(config Config) (*Client, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(config.BaseURL), "/")
	if baseURL == "" {
		if config.BaseURLError != nil {
			return nil, config.BaseURLError
		}
		return nil, errors.New("planehttp: base URL is required")
	}
	if strings.TrimSpace(config.Token) == "" && !config.AllowNoToken {
		if config.TokenError != nil {
			return nil, config.TokenError
		}
		return nil, errors.New("planehttp: bearer token is required")
	}
	client := config.Client
	if client == nil {
		client = &http.Client{Timeout: config.Timeout}
	}
	return &Client{baseURL: baseURL, token: config.Token, client: client, retry: config.Retry}, nil
}

// BaseURL returns the normalized daemon API root.
func (c *Client) BaseURL() string { return c.baseURL }

// HTTPClient returns the configured transport client.
func (c *Client) HTTPClient() *http.Client { return c.client }

// WithHTTPClient returns a copy that uses client for requests.
func (c *Client) WithHTTPClient(client *http.Client) *Client {
	clone := *c
	clone.client = client
	return &clone
}

// DoRaw builds and sends one authenticated request.
func (c *Client) DoRaw(ctx context.Context, method, path string, body io.Reader, headers http.Header) (*http.Response, error) {
	endpoint := c.baseURL + path
	request, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, &RequestError{Op: "build", Endpoint: endpoint, Err: err}
	}
	for name, values := range headers {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	if c.token != "" {
		request.Header.Set("Authorization", "Bearer "+c.token)
	}
	response, err := c.client.Do(request)
	if err != nil {
		return nil, &RequestError{Op: "do", Endpoint: endpoint, Err: err}
	}
	return response, nil
}

// DoJSON marshals body, marks it as JSON, and sends one authenticated request.
func (c *Client) DoJSON(ctx context.Context, method, path string, body any, headers http.Header) (*http.Response, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, &RequestError{Op: "encode", Endpoint: c.baseURL + path, Err: err}
	}
	if headers == nil {
		headers = make(http.Header)
	} else {
		headers = headers.Clone()
	}
	headers.Set("Content-Type", "application/json")
	return c.DoRaw(ctx, method, path, bytes.NewReader(payload), headers)
}

// ReadBounded reads at most limit bytes from body.
func ReadBounded(body io.Reader, limit int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(body, limit))
}

// ErrorFactory preserves the caller's package-specific public error type.
type ErrorFactory func(status int, code, message string) error

// ErrorFallback controls decoding when a response has no valid error envelope.
type ErrorFallback struct {
	Code        string
	CodePrefix  string
	Message     string
	DetailLimit int
	Ellipsis    string
}

// DecodeError decodes the shared error envelope or applies fallback policy.
func DecodeError(status int, raw []byte, factory ErrorFactory, fallback ErrorFallback) error {
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &envelope) == nil && envelope.Error.Code != "" {
		return factory(status, envelope.Error.Code, envelope.Error.Message)
	}
	code := fallback.Code
	if code == "" {
		code = fallback.CodePrefix + strconv.Itoa(status)
	}
	message := fallback.Message
	if message == "" {
		message = strings.TrimSpace(string(raw))
		if fallback.DetailLimit > 0 && len(message) > fallback.DetailLimit {
			cut := fallback.DetailLimit
			for cut > 0 && !utf8.RuneStart(message[cut]) {
				cut--
			}
			message = message[:cut] + fallback.Ellipsis
		}
	}
	return factory(status, code, message)
}
