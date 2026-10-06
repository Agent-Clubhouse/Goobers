package httpapi

import (
	"net/http"
	"testing"
)

func TestClassifyConnectionLocality(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		forwarded  string
		proxies    []string
		want       string
	}{
		{name: "direct local", remoteAddr: "127.0.0.1:8080", want: connectionLocalityLocal},
		{name: "direct IPv6 local", remoteAddr: "[::1]:8080", want: connectionLocalityLocal},
		{name: "direct remote", remoteAddr: "198.51.100.20:443", want: connectionLocalityRemote},
		{
			name:       "trusted proxy reports local client",
			remoteAddr: "10.0.0.8:443",
			forwarded:  "127.0.0.1",
			proxies:    []string{"10.0.0.0/24"},
			want:       connectionLocalityLocal,
		},
		{
			name:       "trusted proxy chain reports remote client",
			remoteAddr: "10.0.0.8:443",
			forwarded:  "198.51.100.20, 10.0.0.7",
			proxies:    []string{"10.0.0.0/24"},
			want:       connectionLocalityRemote,
		},
		{
			name:       "untrusted spoof cannot appear local",
			remoteAddr: "198.51.100.20:443",
			forwarded:  "127.0.0.1",
			proxies:    []string{"10.0.0.0/24"},
			want:       connectionLocalityRemote,
		},
		{
			name:       "trusted proxy malformed metadata",
			remoteAddr: "10.0.0.8:443",
			forwarded:  "not-an-ip",
			proxies:    []string{"10.0.0.0/24"},
			want:       connectionLocalityUnknown,
		},
		{
			name:       "trusted proxy unavailable metadata",
			remoteAddr: "10.0.0.8:443",
			proxies:    []string{"10.0.0.0/24"},
			want:       connectionLocalityUnknown,
		},
		{name: "unavailable peer", want: connectionLocalityUnknown},
		{name: "malformed peer", remoteAddr: "198.51.100.20", want: connectionLocalityUnknown},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ranges, err := parseTrustedProxyRanges(test.proxies)
			if err != nil {
				t.Fatal(err)
			}
			request, err := http.NewRequest(http.MethodGet, PortalConfigPath, nil)
			if err != nil {
				t.Fatal(err)
			}
			request.RemoteAddr = test.remoteAddr
			if test.forwarded != "" {
				request.Header.Set("X-Forwarded-For", test.forwarded)
			}
			if got := classifyConnectionLocality(request, ranges); got != test.want {
				t.Fatalf("classifyConnectionLocality() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestParseTrustedProxyRangesRejectsInvalidValues(t *testing.T) {
	for _, value := range []string{"", "proxy.internal", "10.0.0.0/99"} {
		t.Run(value, func(t *testing.T) {
			if _, err := parseTrustedProxyRanges([]string{value}); err == nil {
				t.Fatalf("parseTrustedProxyRanges(%q) succeeded", value)
			}
		})
	}
}
