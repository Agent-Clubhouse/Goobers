package onecollector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"
)

// MaxRecords caps the number of records in one request.
const MaxRecords = 1000

// MaxBatchBytes caps the encoded request body at eight MiB.
const MaxBatchBytes = 8 << 20

// MaxNativeBytes caps a native record or embedded JSON value at two MiB.
const MaxNativeBytes = 2 << 20

// ErrDisabled identifies a missing explicit environment opt-in.
var ErrDisabled = errors.New("onecollector: explicit environment opt-in required")

// DeliveryError reports a failure category and status without destination values.
type DeliveryError struct {
	Category string
	Status   int
}

func (e *DeliveryError) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("onecollector: HTTP %d; delivery not confirmed", e.Status)
	}
	return "onecollector: " + e.Category + "; delivery uncertain"
}

// Options holds the resolved settings for one separately enabled route.
type Options struct {
	EnableEnv          string
	Endpoint           string
	RoutingKey         string
	InstrumentationKey string
}

// Enabled requires a declared environment variable with the exact value "1".
func (o Options) Enabled() bool { return o.EnableEnv != "" && os.Getenv(o.EnableEnv) == "1" }

// Envelope contains a wire record and non-wire content-preservation markers.
type Envelope struct {
	PreserveInput      bool            `json:"-"`
	RawDataFields      map[string]bool `json:"-"`
	RawExtensionFields map[string]bool `json:"-"`
	Name               string          `json:"name"`
	Time               string          `json:"time"`
	Ver                string          `json:"ver"`
	IKey               string          `json:"iKey"`
	Ext                map[string]any  `json:"ext"`
	Data               any             `json:"data"`
}

// Serialize produces bounded newline-delimited JSON without HTML escaping.
func Serialize(envelopes []Envelope) ([]byte, error) {
	if len(envelopes) < 1 || len(envelopes) > MaxRecords {
		return nil, errors.New("onecollector: batch record limit")
	}
	var body bytes.Buffer
	encoder := json.NewEncoder(&body)
	encoder.SetEscapeHTML(false)
	for _, envelope := range envelopes {
		if envelope.Name == "" {
			return nil, errors.New("onecollector: event name required")
		}
		if err := encoder.Encode(envelope); err != nil {
			return nil, errors.New("onecollector: serialization")
		}
		if body.Len() > MaxBatchBytes {
			return nil, errors.New("onecollector: batch byte limit")
		}
	}
	return body.Bytes(), nil
}

// SendResult sends one bounded batch and returns its HTTP status without retries.
func SendResult(ctx context.Context, options Options, envelopes []Envelope, transport http.RoundTripper) (int, error) {
	if !options.Enabled() {
		return 0, ErrDisabled
	}
	target, err := url.Parse(options.Endpoint)
	if err != nil || target.Scheme != "https" || target.Host == "" ||
		target.User != nil || target.RawQuery != "" || target.Fragment != "" {
		return 0, errors.New("onecollector: invalid HTTPS endpoint")
	}
	if options.RoutingKey == "" || options.InstrumentationKey == "" {
		return 0, errors.New("onecollector: routing configuration required")
	}
	body, err := Serialize(envelopes)
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	query := target.Query()
	for key, value := range map[string]string{
		"cors": "true", "content-type": "application/x-json-stream", "client-id": "NO_AUTH",
		"client-version": "1DS-Web-JS-3.0.2", "time-delta-to-apply-millis": "use-collector-delta",
		"apikey": options.RoutingKey,
	} {
		query.Set(key, value)
	}
	target.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		return 0, errors.New("onecollector: request construction")
	}
	request.Header.Set("Content-Type", "application/x-json-stream")
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	response, err := client.Do(request)
	if err != nil {
		category := "network"
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			category = "timeout"
		} else if ctx.Err() != nil {
			category = "cancelled"
		}
		return 0, &DeliveryError{Category: category}
	}
	// The status decides delivery; response-body cleanup cannot undo acceptance.
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return response.StatusCode, &DeliveryError{Category: "http", Status: response.StatusCode}
	}
	return response.StatusCode, nil
}
