package providers

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (p *GitHubProvider) do(ctx context.Context, method, endpoint string, body interface{}, out interface{}) error {
	return doStatus(ctx, p, method, endpoint, body, out, nil)
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
	return sendJSONWithPolicy(ctx, restSendPolicy{
		client:              p.Client,
		providerHTTPName:    "github",
		maxTransientRetries: p.maxRetries,
		maxRateLimitRetries: p.maxRateLimitRetries,
		maxRateLimitWait:    p.maxRateLimitWait,
		retryable:           retryable,
		sleep:               p.sleep,
		decorate: func(ctx context.Context, req *http.Request) error {
			token, err := p.resolveToken(ctx)
			if err != nil {
				return err
			}
			req.Header.Set("Accept", accept)
			req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
			if token != "" {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			return nil
		},
		beforeSend: func(ctx context.Context) error {
			if p.quotaGate != nil && !p.quotaGateInClient {
				return p.quotaGate.AcquireQuotaRequest(ctx, ProviderGitHub)
			}
			return nil
		},
		observeResponse:     p.observeQuota,
		refreshRejectedAuth: p.invalidateToken,
		isRateLimited:       isRateLimited,
		planRateLimit:       p.rateLimitPlan,
		observeRateLimit:    p.observeRateLimit,
		handleExhaustedRateLimit: func(resp *http.Response, ev RateLimitEvent) (*http.Response, error) {
			_ = resp.Body.Close()
			return nil, rateLimitErrorFrom(ev)
		},
	}, method, endpoint, body)
}

// getAllPages issues GET requests against endpoint with per_page maximized,
// following the response Link header's rel="next" until the result set is
// exhausted, and invokes onPage with each page's raw JSON body. This is the
// shared paginator (#139): before it, every list/read site consumed only the
// first (default 30-item) page, so a claim breadcrumb, failing check, or
// changes-requested review beyond page 1 was silently invisible.
func (p *GitHubProvider) getAllPages(ctx context.Context, endpoint string, onPage func([]byte) error) error {
	return walkLinkPages(ctx, p.send, endpoint, func(body []byte, _ pageContext) error {
		return onPage(body)
	})
}

// getAllPagesWithContext is getAllPages with each page's response metadata
// exposed to the callback (#3392). A periodic full-history walk needs to honor
// x-ratelimit-remaining *while* it walks — deciding to stop only after a 403
// means the shared credential is already at zero for every other operation in
// the window.
func (p *GitHubProvider) getAllPagesWithContext(ctx context.Context, endpoint string, onPage func([]byte, pageContext) error) error {
	return walkLinkPages(ctx, p.send, endpoint, onPage)
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

// invalidateToken marks a refreshable token source's value rejected, and
// reports whether the source can re-resolve at all.
func (p *GitHubProvider) invalidateToken() bool {
	source, ok := p.tokenSource.(RefreshableTokenSource)
	if !ok {
		return false
	}
	source.Invalidate()
	return true
}

// RefreshCIPollCredential lets ci-poll (#6154) distinguish a refreshable
// credential source from a static token when a long poll receives 401, after
// send()'s own one re-resolve (#6120) was also rejected. A refreshable source
// is invalidated and re-resolved once here, so a refresh failure surfaces to
// ci-poll's bounded retry as an error; any other source is asked again. A
// static-token provider reports that no refresh path exists so the caller
// preserves the terminal auth failure.
func (p *GitHubProvider) RefreshCIPollCredential(ctx context.Context) (bool, error) {
	if p.tokenSource == nil {
		return false, nil
	}
	if source, ok := p.tokenSource.(RefreshableTokenSource); ok {
		source.Invalidate()
	}
	_, err := p.tokenSource.Token(ctx)
	return err == nil, err
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
	// Cached independently of quotaObserver being configured: LastObservedQuota
	// (#4182) must work for every caller, not only one that wired an observer.
	if limit, remaining, ok := quotaFromHeaders(resp.Header); ok {
		p.lastQuota.mu.Lock()
		p.lastQuota.limit, p.lastQuota.remaining, p.lastQuota.known = limit, remaining, true
		p.lastQuota.mu.Unlock()
	}
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
