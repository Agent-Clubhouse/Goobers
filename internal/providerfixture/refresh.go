package providerfixture

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

type refreshBackend interface {
	validate() error
	providerName() string
	repositoryIdentity() Repository
	targetIdentity() (issue, pullRequest string)
	requestSet(HTTPClient) []refreshRequest
	httpClient() HTTPClient
	decorateRequest(*http.Request)
	normalizePath(string) string
	normalizeBody([]byte) (json.RawMessage, error)
	normalizeResponseHeaders(http.Header) map[string]string
}

type refreshRequest struct {
	name             string
	rejectNonSuccess bool
	execute          func(context.Context, HTTPClient) error
}

func refreshWithBackend(ctx context.Context, backend refreshBackend) (Fixture, error) {
	if err := backend.validate(); err != nil {
		return Fixture{}, err
	}
	issue, pullRequest := backend.targetIdentity()
	fixture := Fixture{
		SchemaVersion: SchemaVersion,
		Provider:      backend.providerName(),
		Repository:    backend.repositoryIdentity(),
		Issue:         issue,
		PullRequest:   pullRequest,
	}
	recorder := &recordingClient{
		client:  backend.httpClient(),
		backend: backend,
		fixture: &fixture,
	}
	requests := backend.requestSet(recorder)
	fixture.Exchanges = make([]Exchange, 0, len(requests))
	for _, request := range requests {
		recorder.begin(request.name, request.rejectNonSuccess)
		if err := request.execute(ctx, recorder); err != nil {
			return Fixture{}, err
		}
	}
	return fixture, nil
}

type recordingClient struct {
	client           HTTPClient
	backend          refreshBackend
	fixture          *Fixture
	operation        string
	request          int
	rejectNonSuccess bool
}

func (c *recordingClient) begin(operation string, rejectNonSuccess bool) {
	c.operation = operation
	c.request = 0
	c.rejectNonSuccess = rejectNonSuccess
}

func (c *recordingClient) Do(req *http.Request) (*http.Response, error) {
	c.backend.decorateRequest(req)
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s request: %w", c.operation, err)
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	closeErr := resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	if readErr != nil {
		return nil, fmt.Errorf("read %s response: %w", c.operation, readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close %s response: %w", c.operation, closeErr)
	}
	if len(body) > maxResponseBytes {
		return nil, fmt.Errorf("%s response exceeds %d bytes", c.operation, maxResponseBytes)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		if c.rejectNonSuccess {
			return nil, fmt.Errorf("%s request returned status %d: %s", c.operation, resp.StatusCode, strings.TrimSpace(string(body)))
		}
		return resp, nil
	}
	normalizedBody, err := c.backend.normalizeBody(body)
	if err != nil {
		return nil, fmt.Errorf("normalize %s response: %w", c.operation, err)
	}
	c.request++
	name := c.operation
	if c.request > 1 {
		name += "-" + strconv.Itoa(c.request)
	}
	c.fixture.Exchanges = append(c.fixture.Exchanges, Exchange{
		Name:   name,
		Method: req.Method,
		Path:   c.backend.normalizePath(req.URL.RequestURI()),
		Response: FixtureResponse{
			Status:  resp.StatusCode,
			Headers: c.backend.normalizeResponseHeaders(resp.Header),
			Body:    normalizedBody,
		},
	})
	return resp, nil
}
