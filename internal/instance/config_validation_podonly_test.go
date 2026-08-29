package instance

import "testing"

// Regression for #3702/#3703: a daemon with NO human API surface must be able
// to serve off-loopback. Requiring api.auth.oidc specifically made the most
// restrictive posture the only one that could not be expressed — pod tokens
// chained in front of DenyAllAuthenticator authenticate every admitted request.
func TestAPIOffLoopbackNeedsTLSButNotOIDC(t *testing.T) {
	podOnly := APIConfig{
		Listen:          "0.0.0.0:8080",
		TLS:             &APITLSConfig{CertFile: "/c.pem", KeyFile: "/k.pem"},
		PodTokenKeyFile: "/mnt/pod-token/pod-token.key",
	}
	if err := podOnly.validate(podOnly.Listen); err != nil {
		t.Fatalf("off-loopback with TLS and no OIDC must be accepted: %v", err)
	}

	noTLS := APIConfig{Listen: "0.0.0.0:8080"}
	if err := noTLS.validate(noTLS.Listen); err == nil {
		t.Fatal("off-loopback without TLS must still be refused — encryption is unconditional")
	}

	loopback := APIConfig{Listen: "127.0.0.1:8080"}
	if err := loopback.validate(loopback.Listen); err != nil {
		t.Fatalf("the tier-1 loopback default must stay valid: %v", err)
	}
}
