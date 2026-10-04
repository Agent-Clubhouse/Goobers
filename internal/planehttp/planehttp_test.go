package planehttp

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type testError struct {
	status  int
	code    string
	message string
}

func (e *testError) Error() string { return e.code + ": " + e.message }

func TestNewValidatesNormalizesAndDefaults(t *testing.T) {
	baseErr := errors.New("missing base")
	tokenErr := errors.New("missing token")
	if _, err := New(Config{Token: "t", BaseURLError: baseErr}); !errors.Is(err, baseErr) {
		t.Fatalf("missing base error = %v", err)
	}
	if _, err := New(Config{BaseURL: "http://daemon", TokenError: tokenErr}); !errors.Is(err, tokenErr) {
		t.Fatalf("missing token error = %v", err)
	}
	client, err := New(Config{BaseURL: " http://daemon/// ", Token: "t", Timeout: 17 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if client.BaseURL() != "http://daemon" {
		t.Fatalf("base URL = %q", client.BaseURL())
	}
	if client.HTTPClient().Timeout != 17*time.Second {
		t.Fatalf("timeout = %v", client.HTTPClient().Timeout)
	}
	if _, err := New(Config{BaseURL: "http://daemon", AllowNoToken: true}); err != nil {
		t.Fatalf("anonymous client: %v", err)
	}
}

func TestDoRawAndDoJSONSetPlaneHeaders(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests++
		if request.Header.Get("Authorization") != "Bearer token" {
			t.Errorf("authorization = %q", request.Header.Get("Authorization"))
		}
		if requests == 2 {
			if request.Header.Get("Content-Type") != "application/json" {
				t.Errorf("content type = %q", request.Header.Get("Content-Type"))
			}
			raw, _ := io.ReadAll(request.Body)
			if string(raw) != `{"value":"ok"}` {
				t.Errorf("body = %s", raw)
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	client, err := New(Config{BaseURL: server.URL + "/", Token: "token"})
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.DoRaw(context.Background(), http.MethodGet, "/raw", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	response, err = client.DoJSON(context.Background(), http.MethodPost, "/json", struct {
		Value string `json:"value"`
	}{Value: "ok"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
}

func TestReadBoundedAndDecodeError(t *testing.T) {
	raw, err := ReadBounded(strings.NewReader("abcdef"), 3)
	if err != nil || string(raw) != "abc" {
		t.Fatalf("ReadBounded = %q, %v", raw, err)
	}
	factory := func(status int, code, message string) error {
		return &testError{status: status, code: code, message: message}
	}
	err = DecodeError(http.StatusForbidden, []byte(`{"error":{"code":"scope","message":"denied"}}`), factory, ErrorFallback{})
	var decoded *testError
	if !errors.As(err, &decoded) || decoded.status != http.StatusForbidden || decoded.code != "scope" || decoded.message != "denied" {
		t.Fatalf("decoded error = %#v", decoded)
	}
	err = DecodeError(http.StatusBadGateway, []byte("  "+strings.Repeat("x", 8)+"  "), factory, ErrorFallback{
		CodePrefix: "http_", DetailLimit: 5, Ellipsis: "...",
	})
	if !errors.As(err, &decoded) || decoded.code != "http_502" || decoded.message != "xxxxx..." {
		t.Fatalf("fallback error = %#v", decoded)
	}
}
