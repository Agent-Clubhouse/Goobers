package dispatcher

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/goobers/goobers/internal/runnercap"
)

// ServiceReader is the narrow API access needed to resolve in-cluster endpoints.
type ServiceReader interface {
	GetService(context.Context, string, string) (*corev1.Service, error)
}

func networkNone(runner RunnerSpec) bool {
	return slices.Contains(runner.Restrictions, string(runnercap.RestrictionNetworkNone))
}

func serviceEndpointHosts(cfg Config) ([]string, error) {
	var hosts []string
	for _, endpoint := range []string{cfg.WriteAPIBase, cfg.BlobEndpoint} {
		parsed, err := url.Parse(endpoint)
		if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Hostname() == "" {
			return nil, fmt.Errorf("dispatcher: NETWORK_NONE_SERVICE: daemon and blob endpoints must be absolute HTTP(S) Service URLs")
		}
		host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
		if _, _, err := serviceIdentity(host); err != nil {
			return nil, err
		}
		if !slices.Contains(hosts, host) {
			hosts = append(hosts, host)
		}
	}
	slices.Sort(hosts)
	return hosts, nil
}

func serviceIdentity(host string) (string, string, error) {
	parts := strings.Split(host, ".")
	if net.ParseIP(host) != nil || len(parts) < 2 || parts[0] == "" || parts[1] == "" || (len(parts) > 2 && parts[2] != "svc") {
		return "", "", fmt.Errorf("dispatcher: NETWORK_NONE_SERVICE: endpoint host %q must name an in-cluster Service as service.namespace[.svc[.cluster-domain]]", host)
	}
	return parts[1], parts[0], nil
}

func (d *Dispatcher) configWithServiceAliases(ctx context.Context, runner RunnerSpec) (Config, error) {
	cfg := d.cfg
	if !networkNone(runner) {
		return cfg, nil
	}
	reader, ok := d.pods.(ServiceReader)
	if !ok {
		return cfg, fmt.Errorf("dispatcher: NETWORK_NONE_SERVICE: pod API cannot resolve in-cluster Services")
	}
	hosts, err := serviceEndpointHosts(cfg)
	if err != nil {
		return cfg, err
	}
	cfg.NetworkNoneHostAliases = nil
	for _, host := range hosts {
		namespace, name, _ := serviceIdentity(host)
		service, err := reader.GetService(ctx, namespace, name)
		if err != nil {
			return cfg, fmt.Errorf("dispatcher: NETWORK_NONE_SERVICE: resolve %q: %w", host, err)
		}
		if service.Spec.Type == corev1.ServiceTypeExternalName || net.ParseIP(service.Spec.ClusterIP) == nil {
			return cfg, fmt.Errorf("dispatcher: NETWORK_NONE_SERVICE: %q has no stable Service ClusterIP (headless and ExternalName Services are unsupported)", host)
		}
		// Use the Service's primary ClusterIP, matching the existing single-stack
		// endpoint grant. Re-read at every render so a recreated Service cannot
		// leave a stale cached host mapping in a new stage pod.
		cfg.NetworkNoneHostAliases = append(cfg.NetworkNoneHostAliases, corev1.HostAlias{IP: service.Spec.ClusterIP, Hostnames: []string{host}})
	}
	return cfg, nil
}

func stampServiceAliases(cfg Config, spec *corev1.PodSpec, runner RunnerSpec) error {
	if !networkNone(runner) {
		return nil
	}
	hosts, err := serviceEndpointHosts(cfg)
	if err != nil {
		return err
	}
	for _, host := range hosts {
		ip := ""
		for _, alias := range cfg.NetworkNoneHostAliases {
			if slices.Contains(alias.Hostnames, host) && net.ParseIP(alias.IP) != nil {
				if ip != "" && ip != alias.IP {
					return fmt.Errorf("dispatcher: NETWORK_NONE_SERVICE: conflicting Service addresses for %q", host)
				}
				ip = alias.IP
			}
		}
		if ip == "" {
			return fmt.Errorf("dispatcher: NETWORK_NONE_SERVICE: no verified Service ClusterIP for %q; refusing network:none without a host alias", host)
		}
		// Template-supplied mappings cannot redirect dispatcher-owned endpoints.
		for i := range spec.HostAliases {
			spec.HostAliases[i].Hostnames = slices.DeleteFunc(spec.HostAliases[i].Hostnames, func(name string) bool { return strings.EqualFold(strings.TrimSuffix(name, "."), host) })
		}
		spec.HostAliases = slices.DeleteFunc(spec.HostAliases, func(alias corev1.HostAlias) bool { return len(alias.Hostnames) == 0 })
		spec.HostAliases = append(spec.HostAliases, corev1.HostAlias{IP: ip, Hostnames: []string{host}})
	}
	return nil
}
