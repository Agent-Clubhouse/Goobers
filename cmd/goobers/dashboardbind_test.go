package main

import (
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/instance"
)

// The portal sits in front of a control plane, not a read-only view, so the
// bind gate mirrors validateAPIConfig exactly: loopback always, anything else
// only when the instance is both encrypted and authenticated, no override.
func TestDashboardHostGate(t *testing.T) {
	t.Parallel()
	authed := &instance.Config{}
	authed.API.TLS = &instance.APITLSConfig{CertFile: "c.pem", KeyFile: "k.pem"}
	authed.API.Auth = &instance.APIAuthConfig{}

	for _, tc := range []struct {
		name    string
		host    string
		cfg     *instance.Config
		want    string
		wantErr string
	}{
		{name: "empty defaults to loopback", host: "", want: "127.0.0.1"},
		{name: "explicit loopback ip", host: "127.0.0.1", want: "127.0.0.1"},
		{name: "localhost", host: "localhost", want: "localhost"},
		{name: "ipv6 loopback", host: "::1", want: "::1"},
		{
			name: "wildcard refused without tls and auth", host: "0.0.0.0",
			wantErr: "requires",
		},
		{
			name: "public host refused without tls and auth", host: "10.0.0.5",
			wantErr: "no insecure override",
		},
		{name: "wildcard allowed once encrypted and authenticated", host: "0.0.0.0", cfg: authed, want: "0.0.0.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := dashboardHost(tc.host, tc.cfg)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("dashboardHost(%q) = %q, want an error naming the requirement", tc.host, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want it to mention %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("dashboardHost(%q): %v", tc.host, err)
			}
			if got != tc.want {
				t.Fatalf("dashboardHost(%q) = %q, want %q", tc.host, got, tc.want)
			}
		})
	}
}
