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
// discard it). It reuses doRetryable, so GraphQL requests get the same
// token resolution and rate-limit backoff as every REST call — the only
// things it adds are the errors-array check GraphQL needs and REST does
// not, and an explicit retry-safety override: GraphQL's wire method is
// always POST regardless of whether the operation is a read or a write, so
// the literal-method classification sendWithAccept normally uses would
// wrongly deny transient-retry to a genuinely safe-to-retry query (e.g.
// PollMergeQueueEntry) — isGraphQLMutation tells the two apart from the
// query text itself instead (#2026).
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
	if err := p.doRetryable(ctx, http.MethodPost, endpoint, body, &envelope, !isGraphQLMutation(query)); err != nil {
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

// isGraphQLMutation reports whether query is a GraphQL mutation document
// (a write, and so not safe to retry blindly after a transport error or
// 5xx — #2026) rather than a query document (a read, exactly as safe to
// retry as a REST GET). Every query/mutation constant in this package
// opens with the explicit "query"/"mutation" keyword GraphQL's own syntax
// requires for a named or shorthand-with-variables operation, so a prefix
// check on the trimmed document is reliable here without a full parser.
func isGraphQLMutation(query string) bool {
	return strings.HasPrefix(strings.TrimSpace(query), "mutation")
}
