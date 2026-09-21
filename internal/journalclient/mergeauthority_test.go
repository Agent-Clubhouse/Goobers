package journalclient

import "testing"

func TestAnonymousMergeAuthorityRequiresLiteralLoopback(t *testing.T) {
	for _, endpoint := range []string{"http://localhost:7000", "http://example.test", "https://127.0.0.1", "http://user@127.0.0.1", "http://192.0.2.1"} {
		if _, err := NewHTTP(HTTPConfig{BaseURL: endpoint, RunID: "run-1", AllowAnonymousLoopback: true}); err == nil {
			t.Errorf("accepted anonymous endpoint %q", endpoint)
		}
	}
	for _, endpoint := range []string{"http://127.0.0.1:7000", "http://[::1]:7000"} {
		if _, err := NewHTTP(HTTPConfig{BaseURL: endpoint, RunID: "run-1", AllowAnonymousLoopback: true}); err != nil {
			t.Errorf("refused loopback: %v", err)
		}
		if _, err := NewHTTP(HTTPConfig{BaseURL: endpoint, RunID: "run-1"}); err == nil {
			t.Error("ordinary journal client silently opted into anonymous access")
		}
	}
}
