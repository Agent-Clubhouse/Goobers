// Package childadmissionclient is the engine worker's client for the engine
// child-admission plane (#7879). Workers hold no database credentials; the
// control plane owns generated-child custody and serves it over this route.
package childadmissionclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/childresult"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/httpapi"
)

const (
	defaultTimeout   = 30 * time.Second
	maxReceiptBytes  = 64 << 10
	maxManifestReply = httpapi.MaxEngineChildManifestBytes*4/3 + 4<<10
)

// ErrCustodyUnmapped reports an operation this plane does not serve yet.
// Callers fail closed; the engine keeps child execution refused meanwhile.
var ErrCustodyUnmapped = errors.New("childadmissionclient: child custody operation is not served by the control plane yet")

// Client implements engine.ChildService over the plane. TokenSource mints a
// short-lived worker child-admission credential per request.
type Client struct {
	BaseURL     string
	TokenSource func() (string, error)
	HTTP        *http.Client
}

var _ engine.ChildService = (*Client)(nil)

// Admit records one idempotent child start intent.
func (c *Client) Admit(ctx context.Context, in engine.ChildAdmissionInput) (engine.ChildRunBinding, error) {
	var out engine.ChildRunBinding
	return out, c.call(ctx, apicontract.EngineChildAdmitPath, in, http.StatusOK, maxReceiptBytes, &out)
}

// Load returns the exact frozen manifest bytes; the engine verifies digests.
func (c *Client) Load(ctx context.Context, b engine.ChildRunBinding) ([]byte, error) {
	var out httpapi.EngineChildManifest
	if err := c.call(ctx, apicontract.EngineChildLoadPath, b, http.StatusOK, maxManifestReply, &out); err != nil {
		return nil, err
	}
	return out.Manifest, nil
}

// Cancel fences the family. It is idempotent on the control plane.
func (c *Client) Cancel(ctx context.Context, b engine.ChildRunBinding) error {
	return c.call(ctx, apicontract.EngineChildCancelPath, b, http.StatusNoContent, 0, nil)
}

// Park is not served yet.
func (c *Client) Park(context.Context, engine.ChildParkInput) error { return ErrCustodyUnmapped }

// Finalize is not served yet.
func (c *Client) Finalize(context.Context, engine.ChildFinalizeInput) (childresult.TerminalReturn, error) {
	return childresult.TerminalReturn{}, ErrCustodyUnmapped
}

// Complete is not served yet.
func (c *Client) Complete(context.Context, engine.ChildCompletionInput) (apiv1.ContextPointer, error) {
	return apiv1.ContextPointer{}, ErrCustodyUnmapped
}

func (c *Client) call(ctx context.Context, path string, body any, want int, limit int64, out any) error {
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" || c.TokenSource == nil {
		return errors.New("childadmissionclient: plane endpoint and worker credential are required")
	}
	token, err := c.TokenSource()
	if err != nil || token == "" {
		return errors.Join(errors.New("childadmissionclient: worker credential unavailable"), err)
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	client := http.Client{Timeout: defaultTimeout}
	if c.HTTP != nil {
		client = *c.HTTP
	}
	// Keep the worker credential on the configured endpoint.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("childadmissionclient: %s: %w", path, err)
		}
		// The plane may be mid-failover; the engine retries for its budget.
		return fmt.Errorf("childadmissionclient: %s: %w: %w", path, engine.ErrChildCustodyUnavailable, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != want {
		var envelope apicontract.ErrorEnvelope
		_ = json.NewDecoder(io.LimitReader(response.Body, 4<<10)).Decode(&envelope)
		failure := fmt.Errorf("childadmissionclient: %s refused with status %d (%s): %s", path, response.StatusCode, envelope.Error.Code, envelope.Error.Message)
		if class := statusClass(response.StatusCode); class != nil {
			return fmt.Errorf("%w: %w", class, failure)
		}
		return failure
	}
	if out == nil {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > limit {
		return errors.New("childadmissionclient: response exceeds its byte budget")
	}
	return json.Unmarshal(data, out)
}

// statusClass maps a plane status onto the engine's custody error classes.
// The plane answers 503 while no controller holds custody (failover) and a
// 4xx for a request that can never succeed. Credential failures (401/403)
// and other 5xx statuses stay unclassified: bounded retry, then stop.
func statusClass(status int) error {
	switch {
	case status == http.StatusServiceUnavailable, status == http.StatusBadGateway,
		status == http.StatusGatewayTimeout, status == http.StatusTooManyRequests:
		return engine.ErrChildCustodyUnavailable
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return nil
	case status >= 400 && status < 500:
		return engine.ErrChildCustodyRefused
	default:
		return nil
	}
}
