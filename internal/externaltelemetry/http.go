package externaltelemetry

import (
	"fmt"
	"net/http"
)

// httpClient is intentionally private: factories receive only the host-wrapped
// request method rather than the underlying transport.
type httpClient struct {
	client *http.Client
}

// Do executes one network-policy-checked request.
func (c *httpClient) Do(request *http.Request) (*http.Response, error) {
	return c.client.Do(request)
}

func policyClient(base *http.Client, policy NetworkPolicy) *httpClient {
	transport := http.DefaultTransport
	if base != nil && base.Transport != nil {
		transport = base.Transport
	}
	client := &http.Client{Transport: networkPolicyTransport{policy: policy, next: transport}}
	if base != nil {
		client.CheckRedirect = base.CheckRedirect
		client.Jar = base.Jar
	}
	return &httpClient{client: client}
}

type networkPolicyTransport struct {
	policy NetworkPolicy
	next   http.RoundTripper
}

func (t networkPolicyTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if !t.policy.Allows(request.URL) {
		return nil, fmt.Errorf("external telemetry network policy denies %q", request.URL.Redacted())
	}
	return t.next.RoundTrip(request)
}
