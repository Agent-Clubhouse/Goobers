package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/httpapi"
)

type daemonJSONCall[Req any] struct {
	Context         context.Context
	Endpoint        string
	RouteID         apicontract.RouteID
	PathValues      map[string]string
	IdempotencyKey  string
	Input           Req
	MaxResponseBody int64
	ClientTimeout   time.Duration
	AcceptJSON      bool
	EncodePrefix    string
	BuildPrefix     string
	CallPrefix      string
	DecodePrefix    string
}

func callDaemonJSON[Req, Resp any](call daemonJSONCall[Req]) (Resp, *apicontract.APIError, error) {
	var zero Resp
	route, ok := apicontract.V1Route(call.RouteID)
	if !ok {
		return zero, nil, fmt.Errorf("API route %q is not registered", call.RouteID)
	}
	replacements := make([]string, 0, 2*len(call.PathValues))
	for placeholder, value := range call.PathValues {
		replacements = append(replacements, placeholder, url.PathEscape(value))
	}
	routePath := strings.NewReplacer(replacements...).Replace(route.Path)
	body, err := json.Marshal(call.Input)
	if err != nil {
		return zero, nil, fmt.Errorf("%s: %w", call.EncodePrefix, err)
	}
	request, err := http.NewRequestWithContext(call.Context, route.Method, call.Endpoint+routePath, bytes.NewReader(body))
	if err != nil {
		return zero, nil, fmt.Errorf("%s: %w", call.BuildPrefix, err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(httpapi.HeaderIdempotencyKey, call.IdempotencyKey)
	if call.AcceptJSON {
		request.Header.Set("Accept", "application/json")
	}
	if token := strings.TrimSpace(os.Getenv("GOOBERS_API_TOKEN")); token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}

	client := &http.Client{
		Timeout: call.ClientTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	response, err := client.Do(request)
	if err != nil {
		return zero, nil, fmt.Errorf("%s: %w", call.CallPrefix, err)
	}
	defer func() { _ = response.Body.Close() }()
	decoder := json.NewDecoder(io.LimitReader(response.Body, call.MaxResponseBody))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var envelope apicontract.ErrorEnvelope
		if err := decoder.Decode(&envelope); err != nil {
			return zero, nil, fmt.Errorf("daemon API returned %s with an invalid error body: %w", response.Status, err)
		}
		return zero, &envelope.Error, nil
	}
	var decoded Resp
	if err := decoder.Decode(&decoded); err != nil {
		return zero, nil, fmt.Errorf("%s: %w", call.DecodePrefix, err)
	}
	return decoded, nil, nil
}
