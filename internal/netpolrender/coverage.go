package netpolrender

import (
	"fmt"
	"math/big"
	"net"
	"sort"
)

// Coverage math — the address-unit half of the coverage ratchet (issue #3568
// must-carry control 2). UNITS ARE NON-NEGOTIABLE: coverage is measured in
// ADDRESSES, never CIDR-block counts. "4 of 15 blocks" reads as
// mostly-excluded while those 4 blocks are 10,240 of 10,251 addresses —
// block-count views always flatter the exclusion because the aggregates are
// the only non-/32 entries. That block-count framing produced a real
// false-green once (Goobernetes-Infra tools/render-egress-cidrs), which is
// why every number this file produces is an address count.

// addrRange is one inclusive [start, end] address interval within a family.
type addrRange struct {
	start, end *big.Int
}

// addrSet is a normalized (sorted, non-overlapping) set of address intervals,
// kept per family so IPv4 and IPv6 never intersect by accident.
type addrSet struct {
	v4, v6 []addrRange
}

// parseAddrSet converts CIDRs into a normalized addrSet.
func parseAddrSet(cidrs []string) (*addrSet, error) {
	set := &addrSet{}
	for _, cidr := range cidrs {
		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, fmt.Errorf("CIDR %q does not parse: %w", cidr, err)
		}
		r, v4 := cidrRange(ipNet)
		if v4 {
			set.v4 = append(set.v4, r)
		} else {
			set.v6 = append(set.v6, r)
		}
	}
	set.v4 = normalize(set.v4)
	set.v6 = normalize(set.v6)
	return set, nil
}

// cidrRange converts one parsed CIDR into its inclusive address interval and
// reports whether it is IPv4.
func cidrRange(ipNet *net.IPNet) (addrRange, bool) {
	ip := ipNet.IP
	v4 := ip.To4() != nil
	if v4 {
		ip = ip.To4()
	} else {
		ip = ip.To16()
	}
	ones, bits := ipNet.Mask.Size()
	start := new(big.Int).SetBytes(ip)
	size := new(big.Int).Lsh(big.NewInt(1), uint(bits-ones))
	end := new(big.Int).Add(start, size)
	end.Sub(end, big.NewInt(1))
	return addrRange{start: start, end: end}, v4
}

// normalize sorts intervals and merges overlapping/adjacent ones.
func normalize(ranges []addrRange) []addrRange {
	if len(ranges) == 0 {
		return nil
	}
	sort.Slice(ranges, func(i, j int) bool { return ranges[i].start.Cmp(ranges[j].start) < 0 })
	out := []addrRange{ranges[0]}
	for _, r := range ranges[1:] {
		last := &out[len(out)-1]
		// Merge when r starts at or before last.end+1.
		boundary := new(big.Int).Add(last.end, big.NewInt(1))
		if r.start.Cmp(boundary) <= 0 {
			if r.end.Cmp(last.end) > 0 {
				last.end = r.end
			}
			continue
		}
		out = append(out, r)
	}
	return out
}

// intersect returns the normalized intersection of two normalized interval
// lists.
func intersect(a, b []addrRange) []addrRange {
	var out []addrRange
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		start := maxBig(a[i].start, b[j].start)
		end := minBig(a[i].end, b[j].end)
		if start.Cmp(end) <= 0 {
			out = append(out, addrRange{start: start, end: end})
		}
		if a[i].end.Cmp(b[j].end) < 0 {
			i++
		} else {
			j++
		}
	}
	return out
}

// count sums the address count of normalized intervals.
func count(ranges []addrRange) *big.Int {
	total := new(big.Int)
	for _, r := range ranges {
		size := new(big.Int).Sub(r.end, r.start)
		size.Add(size, big.NewInt(1))
		total.Add(total, size)
	}
	return total
}

// IntersectAddresses returns the address count of the intersection with
// other, families kept apart.
func (s *addrSet) IntersectAddresses(other *addrSet) *big.Int {
	return new(big.Int).Add(count(intersect(s.v4, other.v4)), count(intersect(s.v6, other.v6)))
}

func maxBig(a, b *big.Int) *big.Int {
	if a.Cmp(b) >= 0 {
		return a
	}
	return b
}

func minBig(a, b *big.Int) *big.Int {
	if a.Cmp(b) <= 0 {
		return a
	}
	return b
}

// ModelEndpointCoverage computes, per class, how many ADDRESSES of the
// model-endpoint reference set (the union of every GroupKindModel group's
// CIDRs) the class's granted CIDR set covers. A network:none class grants no
// CIDRs and covers zero. The number is what the ratchet freezes: a meta
// rotation that silently widens an aggregate raises it, and the baseline
// comparison fails on the rise.
func ModelEndpointCoverage(classes []Class, groups []AllowlistGroup) (map[string]*big.Int, error) {
	var modelCIDRs []string
	for _, group := range groups {
		if group.Kind == GroupKindModel {
			modelCIDRs = append(modelCIDRs, group.CIDRs...)
		}
	}
	modelSet, err := parseAddrSet(modelCIDRs)
	if err != nil {
		return nil, fmt.Errorf("model-endpoint reference set: %w", err)
	}

	var grantedCIDRs []string
	for _, group := range groups {
		grantedCIDRs = append(grantedCIDRs, group.CIDRs...)
	}
	grantedSet, err := parseAddrSet(grantedCIDRs)
	if err != nil {
		return nil, fmt.Errorf("granted set: %w", err)
	}
	grantedCoverage := grantedSet.IntersectAddresses(modelSet)

	out := make(map[string]*big.Int, len(classes))
	for _, class := range classes {
		if class.NetworkNone {
			out[class.Value] = new(big.Int)
			continue
		}
		// Every non-none class grants the full configured allowlist in v1;
		// per-class narrowing (a stricter CIDR set per class, restrictions
		// doc §6) slots in here without changing the baseline format.
		out[class.Value] = new(big.Int).Set(grantedCoverage)
	}
	return out, nil
}
