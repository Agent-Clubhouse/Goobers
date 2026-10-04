package providers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const adoReadPlanMaxBytes = 256 << 10

type adoListReadKey struct{}
type adoReadPlanKey struct{}
type adoWIQLReadRequest struct {
	Query string `json:"query"`
}
type adoReadPlan struct {
	endpoint string
	digest   string
}

// declaredADOReadContext marks only provider-built list query/hydration bodies.
// Mutation preflights and marker/idempotency scans do not opt into shared reads.
func declaredADOReadContext(ctx context.Context, method, endpoint string, body any) context.Context {
	if enabled, _ := ctx.Value(adoListReadKey{}).(bool); !enabled || method != http.MethodPost {
		return ctx
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.User != nil {
		return ctx
	}
	suffix := ""
	switch value := body.(type) {
	case adoWIQLReadRequest:
		if value.Query == "" {
			return ctx
		}
		suffix = adoWIQLPath
	case adoWorkItemsBatchRequest:
		if len(value.IDs) == 0 || len(value.IDs) > adoWorkItemsBatchSize || value.Expand != "Relations" || value.ErrorPolicy != "Omit" {
			return ctx
		}
		suffix = adoWorkItemsBatchPath
	default:
		return ctx
	}
	if !strings.HasSuffix(parsed.Path, suffix) {
		return ctx
	}
	raw, err := json.Marshal(body)
	if err != nil || len(raw) > adoReadPlanMaxBytes {
		return ctx
	}
	sum := sha256.Sum256(raw)
	return context.WithValue(ctx, adoReadPlanKey{}, adoReadPlan{endpoint: endpoint, digest: hex.EncodeToString(sum[:])})
}

// ADOReadPlanDigest recognizes an internal provider declaration and verifies the
// exact bounded request body. HTTP headers or caller JSON cannot declare a POST
// cacheable. The digest preserves WIQL literals, batch membership and expansion.
func ADOReadPlanDigest(req *http.Request) (string, bool) {
	if req == nil || req.Method != http.MethodPost || req.URL == nil || req.URL.User != nil || req.GetBody == nil {
		return "", false
	}
	plan, ok := req.Context().Value(adoReadPlanKey{}).(adoReadPlan)
	if !ok || plan.endpoint != req.URL.String() {
		return "", false
	}
	body, err := req.GetBody()
	if err != nil {
		return "", false
	}
	defer func() { _ = body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(body, adoReadPlanMaxBytes+1))
	if err != nil || len(raw) > adoReadPlanMaxBytes {
		return "", false
	}
	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])
	return digest, digest == plan.digest
}
