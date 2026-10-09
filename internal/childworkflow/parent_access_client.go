package childworkflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/mcpio"
)

// ParentAccessClient exchanges an exact authenticated parent contract for child tools.
type ParentAccessClient struct {
	Endpoint, Token, ContractDigest string
	Client                          *http.Client
}

func (c ParentAccessClient) request(ctx context.Context, run, method string) ([]byte, error) {
	if !apiv1.ValidRunID(run) || !blobstore.ValidDigest(c.ContractDigest) || c.Token == "" {
		return nil, errors.New("parent grant custody unavailable")
	}
	endpoint, err := url.Parse(c.Endpoint)
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return nil, errors.New("parent grant endpoint invalid")
	}
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/api/v1/runs/" + run + "/child-workflow-access"
	body, _ := json.Marshal(struct {
		ContractDigest string `json:"contractDigest"`
	}{c.ContractDigest})
	req, err := http.NewRequestWithContext(ctx, method, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	client := http.Client{Timeout: 30 * time.Second}
	if c.Client != nil {
		client = *c.Client
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, 8193))
	if err != nil {
		return nil, err
	}
	if len(data) > 8192 {
		return nil, errors.New("parent grant response exceeds bound")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("parent grant request refused (HTTP %d)", response.StatusCode)
	}
	return data, nil
}

// Acquire obtains child tool access bound to this client's parent contract.
func (c ParentAccessClient) Acquire(ctx context.Context, run string) (*mcpio.ChildWorkflowAccess, error) {
	data, err := c.request(ctx, run, http.MethodPost)
	if err != nil {
		return nil, err
	}
	var access mcpio.ChildWorkflowAccess
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&access); err != nil {
		return nil, errors.New("parent grant response invalid")
	}
	if err = decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("parent grant response has trailing data")
	}
	if err = access.Validate(run); err != nil {
		return nil, err
	}
	return &access, nil
}

// Revoke releases child tool access for this client's parent contract.
func (c ParentAccessClient) Revoke(ctx context.Context, run string) error {
	_, err := c.request(ctx, run, http.MethodDelete)
	return err
}
