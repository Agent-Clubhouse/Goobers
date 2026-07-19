package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// githubGraphQLResponse is the envelope every GitHub GraphQL response uses.
// GraphQL reports application-level failures as HTTP 200 with a non-empty
// errors array, so p.do's non-2xx check alone never sees them — graphql
// below is the only correct way to issue one of these requests.
type githubGraphQLResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"errors"`
}

// graphql issues one GitHub GraphQL request against BaseURL's /graphql
// endpoint and decodes the response's "data" object into out (nil to
// discard it). It reuses p.do, so GraphQL requests get the same token
// resolution, rate-limit backoff, and transient-retry behavior as every
// REST call — the only thing it adds is the errors-array check GraphQL
// needs and REST does not.
//
// Used only where GitHub exposes no REST equivalent at all. Merge-queue
// enqueue is the first such case (issue #882): there is no REST endpoint
// that adds a pull request to a merge queue, and the merge endpoint that
// was previously assumed to do so implicitly does not (see
// EnqueuePullRequest's doc).
func (p *GitHubProvider) graphql(ctx context.Context, query string, variables map[string]interface{}, out interface{}) error {
	endpoint, err := joinURL(p.BaseURL, "graphql")
	if err != nil {
		return err
	}
	body := map[string]interface{}{"query": query}
	if len(variables) > 0 {
		body["variables"] = variables
	}
	var envelope githubGraphQLResponse
	if err := p.do(ctx, http.MethodPost, endpoint, body, &envelope); err != nil {
		return err
	}
	if len(envelope.Errors) > 0 {
		messages := make([]string, 0, len(envelope.Errors))
		for _, e := range envelope.Errors {
			if e.Type != "" {
				messages = append(messages, fmt.Sprintf("%s: %s", e.Type, e.Message))
				continue
			}
			messages = append(messages, e.Message)
		}
		return fmt.Errorf("github graphql: %s", strings.Join(messages, "; "))
	}
	if out == nil || len(envelope.Data) == 0 {
		return nil
	}
	if err := json.Unmarshal(envelope.Data, out); err != nil {
		return fmt.Errorf("github graphql: decode data: %w", err)
	}
	return nil
}
