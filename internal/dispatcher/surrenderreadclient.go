package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/daemonclient"
)

// SurrenderReadClient is a resident worker's read-only SurrenderPlane. It uses
// a separate bearer from blob, pod, and config-observability credentials.
type SurrenderReadClient struct {
	BaseURL     string
	TokenSource func() (string, error)
	Client      *http.Client
}

func (c *SurrenderReadClient) fetch(ctx context.Context, run, stage string, attempt int, seen bool) ([]byte, error) {
	if !apiv1.ValidRunID(run) || !apiv1.ValidRunID(stage) || attempt < 1 {
		return nil, errors.New("dispatcher: invalid surrender read identity")
	}
	if c.TokenSource == nil {
		return nil, errors.New("dispatcher: surrender read requires worker authentication")
	}
	token, err := c.TokenSource()
	if err != nil {
		return nil, fmt.Errorf("dispatcher: surrender read authentication: %w", err)
	}
	if token == "" {
		return nil, errors.New("dispatcher: empty surrender worker credential")
	}
	path := strings.TrimRight(c.BaseURL, "/") + "/api/v1/runs/" + run + "/stages/" + stage + "/attempts/" + strconv.Itoa(attempt) + "/surrender"
	limit := int64(MaxSurrenderReadBytes)
	if seen {
		path += "/seen"
		limit = 128
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	client := c.Client
	if client == nil {
		client = daemonclient.NewHTTP(defaultSurrenderTimeout)
	}
	// Keep worker credentials on the configured endpoint, including same-host redirects.
	boundedClient := *client
	boundedClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := boundedClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("dispatcher: read surrender: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusNotFound && !seen {
		return nil, ErrNoSurrender
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("dispatcher: surrender read refused with status %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("dispatcher: surrender response exceeds byte budget")
	}
	return data, nil
}

// Get returns the bounded surrendered result for an attempt.
func (c *SurrenderReadClient) Get(ctx context.Context, run, stage string, attempt int) ([]byte, error) {
	return c.fetch(ctx, run, stage, attempt, false)
}

// Has reports whether the daemon has recorded a surrender for an attempt.
func (c *SurrenderReadClient) Has(ctx context.Context, run, stage string, attempt int) (bool, error) {
	data, err := c.fetch(ctx, run, stage, attempt, true)
	if err != nil {
		return false, err
	}
	var response struct {
		Seen *bool `json:"seen"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return false, errors.New("dispatcher: malformed surrender presence response")
	}
	if response.Seen == nil {
		return false, errors.New("dispatcher: surrender presence response omitted seen")
	}
	return *response.Seen, nil
}

// Put rejects writes; resident worker credentials only authorize reads.
func (c *SurrenderReadClient) Put(context.Context, string, string, int, []byte) error {
	return errors.New("dispatcher: resident worker surrender transport is read-only")
}

// Describe identifies this transport without exposing endpoint credentials.
func (c *SurrenderReadClient) Describe() string { return "worker-surrender-endpoint" }
