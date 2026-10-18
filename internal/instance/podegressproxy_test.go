package instance

import (
	"strings"
	"testing"
)

func TestValidatePodEgressProxy(t *testing.T) {
	good := &PodEgressProxyConfig{HTTPSProxy: "http://proxy.ns:3128", HTTPProxy: "https://proxy.ns:3128", NoProxy: ".svc,10.0.0.0/8"}
	for _, tc := range []struct {
		name string
		p    *PodEgressProxyConfig
		want string
	}{
		{"unset", nil, ""},
		{"valid", good, ""},
		{"empty", &PodEgressProxyConfig{}, "sets none"},
		{"no scheme", &PodEgressProxyConfig{HTTPSProxy: "proxy:3128"}, "http:// or https://"},
		{"bad scheme", &PodEgressProxyConfig{HTTPProxy: "socks5://proxy:1080"}, "http:// or https://"},
		{"no host", &PodEgressProxyConfig{HTTPSProxy: "http://"}, "http:// or https://"},
		{"credentials", &PodEgressProxyConfig{HTTPSProxy: "http://u:p@proxy:3128"}, "credentials"},
		{"whitespace", &PodEgressProxyConfig{NoProxy: ".svc, .local"}, "whitespace"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := RunnerConfig{PodEgressProxy: tc.p}.validatePodEgressProxy()
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}
