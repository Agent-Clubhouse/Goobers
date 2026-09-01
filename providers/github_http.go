package providers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (p *GitHubProvider) do(ctx context.Context, method, endpoint string, body interface{}, out interface{}) error {
	return p.doStatus(ctx, method, endpoint, body, out, nil)
}

// doRetryable is do with sendWithAcceptRetryable's explicit retry-safety
// override — see its doc for why graphql needs this instead of do.
func (p *GitHubProvider) doRetryable(ctx context.Context, method, endpoint string, body, out interface{}, retryable bool) error {
	resp, err := p.sendWithAcceptRetryable(ctx, method, endpoint, body, "application/vnd.github+json", retryable)
	if err != nil {
		return err
	}
	return readJSONResponse(resp, method, endpoint, out)
}

// send issues one GitHub request, retrying transient failures — rate limits,
// 5xx server errors, and transport errors — with independent bounded retry
// budgets. Rate-limit retries also honor maxRateLimitWait total sleep and
// X-RateLimit-Reset/Retry-After. It returns the final
// response for the caller to consume and close; a nil error guarantees a
// non-nil response. A rate limit that cannot be absorbed within those
// budgets returns a typed *RateLimitError (#614) rather than the response,
// so no caller ever folds it into a generic non-2xx string error. Callers
// that only need a decoded body should use doStatus; getAllPages uses send
// directly so it can read the Link header for pagination (#139).
func (p *GitHubProvider) send(ctx context.Context, method, endpoint string, body interface{}) (*http.Response, error) {
	return p.sendWithAccept(ctx, method, endpoint, body, "application/vnd.github+json")
}

func (p *GitHubProvider) sendWithAccept(ctx context.Context, method, endpoint string, body interface{}, accept string) (*http.Response, error) {
	return p.sendWithAcceptRetryable(ctx, method, endpoint, body, accept, isIdempotentHTTPMethod(method))
}

// sendWithAcceptRetryable is sendWithAccept with an explicit retry-safety
// override, for the one caller (graphql) whose wire method (always POST,
// GraphQL's transport requirement) does not match its actual idempotency —
// a GraphQL query (read) is exactly as safe to retry as a REST GET, but the
// literal HTTP method alone can't tell the two apart (#2026).
func (p *GitHubProvider) sendWithAcceptRetryable(ctx context.Context, method, endpoint string, body interface{}, accept string, retryable bool) (*http.Response, error) {
	maxWait := p.maxRateLimitWait
	if maxWait <= 0 {
		maxWait = defaultRateLimitMaxWait
	}
	var rateLimitWaited time.Duration
	var rateLimitRetries, transientRetries int
	for {
		req, err := newJSONRequest(ctx, method, endpoint, body)
		if err != nil {
			return nil, err
		}
		token, err := p.resolveToken(ctx)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", accept)
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if p.quotaGate != nil && !p.quotaGateInClient {
			if err := p.quotaGate.AcquireQuotaRequest(ctx, ProviderGitHub); err != nil {
				return nil, err
			}
		}
		resp, err := httpClientOrDefault(p.Client).Do(req)
		if err != nil {
			// Transport error (connection reset, DNS blip, timeout): retry with
			// backoff rather than fail the stage on a single network hiccup
			// (#139) — but only for idempotent methods (#2026). A POST/PATCH
			// whose response was lost to the transport error may have already
			// committed server-side (issue creation, a comment post, a label
			// mutation); GitHub has no transport-level dedup marker to make a
			// blind retry of those safe, so a lost response on a non-idempotent
			// method is surfaced as an error rather than silently risking a
			// duplicate. No response to close on this path.
			if retryable && transientRetries < p.maxRetries {
				if serr := p.sleep(ctx, backoffDuration(transientRetries)); serr != nil {
					return nil, serr
				}
				transientRetries++
				continue
			}
			return nil, fmt.Errorf("send request: %w", err)
		}
		p.observeQuota(ctx, resp)
		if isRateLimited(resp) {
			wait, ev := p.rateLimitPlan(resp, endpoint, rateLimitRetries)
			_ = resp.Body.Close()
			if rateLimitRetries >= p.maxRateLimitRetries || wait > maxWait-rateLimitWaited {
				// Waiting can't help within this request's budget — the
				// retry allowance is spent, or the reset is further out than
				// the wait budget allows (#614). Fail FAST with the typed
				// error so the caller (and the run journal) sees "rate
				// limited, resets at <t>" instead of a generic 403 string,
				// and no time is burned sleeping toward a wait that cannot
				// reach the reset anyway.
				ev.Outcome = RateLimitOutcomeExhausted
				p.observeRateLimit(ctx, ev)
				return nil, rateLimitErrorFrom(ev)
			}
			if err := p.sleep(ctx, wait); err != nil {
				ev.Outcome = RateLimitOutcomeCanceled
				p.observeRateLimit(ctx, ev)
				return nil, err
			}
			ev.Outcome = RateLimitOutcomeRetry
			p.observeRateLimit(ctx, ev)
			rateLimitWaited += wait
			rateLimitRetries++
			continue
		}
		if resp.StatusCode >= 500 && retryable && transientRetries < p.maxRetries {
			// Server-side error: retry with backoff. GitHub 5xx is usually
			// transient; without this a single blip fails the stage attempt.
			// Restricted to idempotent methods (#2026) for the same reason as
			// the transport-error retry above — a 5xx can follow a request
			// that already committed.
			_ = resp.Body.Close()
			if err := p.sleep(ctx, backoffDuration(transientRetries)); err != nil {
				return nil, err
			}
			transientRetries++
			continue
		}
		return resp, nil
	}
}

// doStatus performs a GitHub request with transient-failure retries (see send).
// Status codes in allowStatus are treated as success (used to tolerate a 404
// when removing a label that is not present); the response body is not decoded
// for those.
func (p *GitHubProvider) doStatus(ctx context.Context, method, endpoint string, body, out interface{}, allowStatus []int) error {
	resp, err := p.send(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	for _, code := range allowStatus {
		if resp.StatusCode == code {
			_ = resp.Body.Close()
			return nil
		}
	}
	return readJSONResponse(resp, method, endpoint, out)
}

// getAllPages issues GET requests against endpoint with per_page maximized,
// following the response Link header's rel="next" until the result set is
// exhausted, and invokes onPage with each page's raw JSON body. This is the
// shared paginator (#139): before it, every list/read site consumed only the
// first (default 30-item) page, so a claim breadcrumb, failing check, or
// changes-requested review beyond page 1 was silently invisible.
func (p *GitHubProvider) getAllPages(ctx context.Context, endpoint string, onPage func([]byte) error) error {
	return p.getAllPagesWithContext(ctx, endpoint, func(body []byte, _ pageContext) error {
		return onPage(body)
	})
}

// pageContext is the per-page metadata getAllPagesWithContext hands a callback
// alongside the body: the response headers (so a walk can price itself against
// the live rate-limit window) and whether another page exists (so a callback
// that stops early can tell "budget exhausted mid-history" from "reached the
// natural end").
type pageContext struct {
	Header  http.Header
	HasNext bool
}

// getAllPagesWithContext is getAllPages with each page's response metadata
// exposed to the callback (#3392). A periodic full-history walk needs to honor
// x-ratelimit-remaining *while* it walks — deciding to stop only after a 403
// means the shared credential is already at zero for every other operation in
// the window.
func (p *GitHubProvider) getAllPagesWithContext(ctx context.Context, endpoint string, onPage func([]byte, pageContext) error) error {
	next, err := withPerPage(endpoint, maxPerPage)
	if err != nil {
		return err
	}
	for next != "" {
		resp, err := p.send(ctx, http.MethodGet, next, nil)
		if err != nil {
			return err
		}
		header := resp.Header.Clone()
		body, nextLink, err := readPage(resp, http.MethodGet, next)
		if err != nil {
			return err
		}
		if err := onPage(body, pageContext{Header: header, HasNext: nextLink != ""}); err != nil {
			if errors.Is(err, errStopPaging) {
				return nil
			}
			return err
		}
		next = nextLink
	}
	return nil
}

// quotaFromHeaders reads the absolute rate-limit window off a provider
// response. A response replayed from the shared snapshot cache spent no quota
// and carries no window, so it reports unknown rather than a stale number.
func quotaFromHeaders(header http.Header) (limit, remaining int, ok bool) {
	if header == nil || header.Get(QuotaCacheHitHeader) == "true" {
		return 0, 0, false
	}
	limit, limitErr := strconv.Atoi(strings.TrimSpace(header.Get("X-RateLimit-Limit")))
	remaining, remainingErr := strconv.Atoi(strings.TrimSpace(header.Get("X-RateLimit-Remaining")))
	if limitErr != nil || remainingErr != nil || limit <= 0 || remaining < 0 {
		return 0, 0, false
	}
	return limit, remaining, true
}

// resolveToken returns the per-request token from the token source when configured,
// falling back to the statically injected token.
func (p *GitHubProvider) resolveToken(ctx context.Context) (string, error) {
	if p.tokenSource != nil {
		return p.tokenSource.Token(ctx)
	}
	return p.Token, nil
}

func (p *GitHubProvider) recordExternalRef(ctx context.Context, ref ExternalRef) {
	if p.recorder != nil {
		p.recorder.RecordExternalRef(ctx, ref)
	}
}

func (p *GitHubProvider) observeRateLimit(ctx context.Context, ev RateLimitEvent) {
	if p.rateObserver != nil {
		p.rateObserver.ObserveRateLimit(ctx, ev)
	}
}

func (p *GitHubProvider) observeQuota(ctx context.Context, resp *http.Response) {
	if p.quotaObserver == nil {
		return
	}
	observation := QuotaObservation{
		Provider: ProviderGitHub,
		Cached:   resp.Header.Get(QuotaCacheHitHeader) == "true",
	}
	remaining, remainingErr := strconv.Atoi(strings.TrimSpace(resp.Header.Get("X-RateLimit-Remaining")))
	resetUnix, resetErr := strconv.ParseInt(strings.TrimSpace(resp.Header.Get("X-RateLimit-Reset")), 10, 64)
	if remainingErr == nil && resetErr == nil && remaining >= 0 && resetUnix > 0 {
		observation.Remaining = remaining
		observation.Reset = time.Unix(resetUnix, 0)
		observation.Known = true
	}
	p.quotaObserver.ObserveQuota(ctx, observation)
}
