package httpapi

import (
	"fmt"
	"net"
	"net/http"
	"strings"
)

const (
	connectionLocalityLocal   = "local"
	connectionLocalityRemote  = "remote"
	connectionLocalityUnknown = "unknown"
)

func parseTrustedProxyRanges(values []string) ([]string, error) {
	ranges := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			return nil, fmt.Errorf("http api trusted proxy entries must be non-empty IP addresses or CIDR ranges")
		}
		if ip := net.ParseIP(value); ip != nil {
			if ip.To4() != nil {
				value += "/32"
			} else {
				value += "/128"
			}
		}
		if _, _, err := net.ParseCIDR(value); err != nil {
			return nil, fmt.Errorf("http api trusted proxy %q must be an IP address or CIDR range", value)
		}
		ranges = append(ranges, value)
	}
	return ranges, nil
}

func classifyConnectionLocality(request *http.Request, trustedProxyRanges []string) string {
	peer, ok := remoteAddressIP(request.RemoteAddr)
	if !ok {
		return connectionLocalityUnknown
	}
	trusted, ok := addressInRanges(peer, trustedProxyRanges)
	if !ok {
		return connectionLocalityUnknown
	}
	if !trusted {
		return localityOf(peer)
	}

	forwarded := request.Header.Values("X-Forwarded-For")
	if len(forwarded) == 0 {
		return connectionLocalityUnknown
	}
	var chain []net.IP
	for _, value := range forwarded {
		for _, part := range strings.Split(value, ",") {
			ip := net.ParseIP(strings.TrimSpace(part))
			if ip == nil {
				return connectionLocalityUnknown
			}
			chain = append(chain, ip)
		}
	}
	if len(chain) == 0 {
		return connectionLocalityUnknown
	}
	for index := len(chain) - 1; index >= 0; index-- {
		trusted, valid := addressInRanges(chain[index], trustedProxyRanges)
		if !valid {
			return connectionLocalityUnknown
		}
		if !trusted || index == 0 {
			return localityOf(chain[index])
		}
	}
	return connectionLocalityUnknown
}

func remoteAddressIP(address string) (net.IP, bool) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, false
	}
	ip := net.ParseIP(host)
	return ip, ip != nil
}

func addressInRanges(ip net.IP, ranges []string) (bool, bool) {
	for _, value := range ranges {
		_, network, err := net.ParseCIDR(value)
		if err != nil {
			return false, false
		}
		if network.Contains(ip) {
			return true, true
		}
	}
	return false, true
}

func localityOf(ip net.IP) string {
	if ip.IsLoopback() {
		return connectionLocalityLocal
	}
	return connectionLocalityRemote
}
