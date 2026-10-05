package providers

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
)

type responseBodyLimitKey struct{}

// ErrResponseBodyLimit reports refusal of an oversized successful REST reply.
// No truncated JSON is passed to provider decoders.
var ErrResponseBodyLimit = errors.New("provider successful response exceeds configured byte limit")

// WithResponseBodyLimit bounds each successful REST response before decoding.
// Ordinary requests are unchanged. A nested positive limit may only narrow an
// existing limit; nonpositive values select the smallest possible limit.
func WithResponseBodyLimit(ctx context.Context, limit int64) context.Context {
	if limit <= 0 {
		limit = 1
	}
	if current, ok := ctx.Value(responseBodyLimitKey{}).(int64); ok && current < limit {
		limit = current
	}
	return context.WithValue(ctx, responseBodyLimitKey{}, limit)
}

func boundSuccessResponse(ctx context.Context, response *http.Response) (*http.Response, error) {
	limit, bounded := ctx.Value(responseBodyLimitKey{}).(int64)
	if !bounded || response.StatusCode < 200 || response.StatusCode > 299 {
		return response, nil
	}
	// Reject an absurd caller limit before limit+1 can overflow. The context
	// option is intentionally for bounded projection reads, not large blobs.
	if limit > 64<<20 || response.ContentLength > limit {
		_ = response.Body.Close()
		return nil, ErrResponseBodyLimit
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	_ = response.Body.Close()
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, ErrResponseBodyLimit
	}
	response.Body = io.NopCloser(bytes.NewReader(body))
	return response, nil
}
