package instance

import (
	"fmt"
	"net/url"
	"strings"
)

// PodEgressProxyConfig is runner.podEgressProxy: the forward proxy the
// dispatcher stamps into stage pods whose runner class carries
// network:allowlist (#6748). Such a pod cannot dial module registries or
// package indexes directly; the instance's FQDN-filtering egress proxy is the
// only path. The daemon and worker pods carry these variables in their own
// environment, but a stage pod inherits nothing, runner classes have no env,
// and an agentic stage has no run.env, so without this setting `go test` in an
// implementer stage times out dialing proxy.golang.org.
//
// A stage's own run.env value for a proxy variable always wins.
type PodEgressProxyConfig struct {
	// HTTPSProxy is stamped as HTTPS_PROXY and https_proxy (git/curl read only
	// the lowercase spelling).
	HTTPSProxy string `json:"httpsProxy,omitempty" yaml:"httpsProxy,omitempty"`
	// HTTPProxy is stamped as HTTP_PROXY and http_proxy.
	HTTPProxy string `json:"httpProxy,omitempty" yaml:"httpProxy,omitempty"`
	// NoProxy is stamped as NO_PROXY and no_proxy: a comma-separated list of
	// hosts, domains and CIDRs that bypass the proxy (the cluster-internal
	// daemon/blob/claims endpoints belong here).
	NoProxy string `json:"noProxy,omitempty" yaml:"noProxy,omitempty"`
}

// Enabled reports whether any proxy variable is configured.
func (p *PodEgressProxyConfig) Enabled() bool {
	return p != nil && (p.HTTPSProxy != "" || p.HTTPProxy != "" || p.NoProxy != "")
}

func (c RunnerConfig) validatePodEgressProxy() error {
	p := c.PodEgressProxy
	if p == nil {
		return nil
	}
	if !p.Enabled() {
		return fmt.Errorf("runner.podEgressProxy is declared but sets none of httpsProxy, httpProxy, noProxy")
	}
	for field, value := range map[string]string{"httpsProxy": p.HTTPSProxy, "httpProxy": p.HTTPProxy} {
		if value == "" {
			continue
		}
		u, err := url.Parse(value)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
			return fmt.Errorf("runner.podEgressProxy.%s %q must be an http:// or https:// URL with a host", field, value)
		}
		if u.User != nil {
			return fmt.Errorf("runner.podEgressProxy.%s must not embed credentials: it is stamped in clear into every allowlisted stage pod", field)
		}
	}
	if strings.ContainsAny(p.NoProxy, " \t\r\n") {
		return fmt.Errorf("runner.podEgressProxy.noProxy %q must be a comma-separated list without whitespace", p.NoProxy)
	}
	return nil
}
