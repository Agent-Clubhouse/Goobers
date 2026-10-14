package providers

import "net/http"

// configuredHTTPClient recovers explicit transport settings through decorators.
// It does not send requests through them. Git discovery copies only Transport
// and a shorter Timeout, preserving its own no-redirect/no-cookie policy.
func configuredHTTPClient(client HTTPClient) *http.Client {
	// Bound traversal even if a third-party decorator returns itself or cycles.
	for depth := 0; depth < 16; depth++ {
		if configured, ok := client.(*http.Client); ok {
			return configured
		}
		wrapper, ok := client.(interface{ UnwrapHTTPClient() HTTPClient })
		if !ok {
			return nil
		}
		client = wrapper.UnwrapHTTPClient()
	}
	return nil
}
