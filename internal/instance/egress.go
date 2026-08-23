package instance

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
)

// EgressConfig is the instance-operator-supplied network destination set the
// per-runner-class NetworkPolicy renderer fills into its manifests (issue
// #3568; goobernetes-restrictions.md §2.2: "the allowlist CIDR set is
// instance-operator-supplied configuration rendered into the manifests").
// It is configuration for RENDERING — the daemon itself never applies cluster
// networking (restrictions doc §7: the operator holds no networking.k8s.io
// RBAC); declaring it changes nothing about local execution.
type EgressConfig struct {
	// Allowlist is the named destination-group list every network-granting
	// runner class is rendered with.
	Allowlist []EgressAllowlistGroup `json:"allowlist,omitempty" yaml:"allowlist,omitempty"`
}

// EgressAllowlistGroup is one named CIDR destination group: the git/backlog
// provider ranges, the model endpoint ranges, or a gaggle's sandbox targets —
// the three destination families of the k8s-infra-shape §5 stage egress
// posture.
type EgressAllowlistGroup struct {
	// Name identifies the group in rendered provenance markers and errors.
	// Lowercase DNS label, unique within the allowlist.
	Name string `json:"name" yaml:"name"`
	// Kind is the destination family: "provider" (git/backlog provider),
	// "model" (model/agent endpoint — the coverage ratchet's reference set),
	// or "sandbox" (the gaggle's declared sandbox/provisioner targets).
	Kind string `json:"kind" yaml:"kind"`
	// Source is the upstream document the CIDRs were transcribed from (e.g.
	// https://api.github.com/meta). Optional: empty declares an
	// operator-local set with no upstream to drift from. When set,
	// SourceSHA256 is required and `goobers netpol-render --check` fails when
	// the live document's hash no longer matches — without that check a stale
	// CIDR set fails mid-run as a connect timeout indistinguishable from a
	// correct denial.
	Source string `json:"source,omitempty" yaml:"source,omitempty"`
	// SourceSHA256 is the hex sha256 of the Source document at transcription
	// time — the provenance marker the drift check validates.
	SourceSHA256 string `json:"sourceSHA256,omitempty" yaml:"sourceSHA256,omitempty"`
	// CIDRs are the granted destination blocks, spelled as exact network
	// addresses (no host bits).
	CIDRs []string `json:"cidrs" yaml:"cidrs"`
	// Ports are the allowed TCP ports; empty defaults to 443.
	Ports []int `json:"ports,omitempty" yaml:"ports,omitempty"`
}

// The egress allowlist group kinds.
const (
	EgressGroupKindProvider = "provider"
	EgressGroupKindModel    = "model"
	EgressGroupKindSandbox  = "sandbox"
)

var sha256HexPattern = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// validateEgress checks the egress: section fail-first at load, like every
// other instance.yaml section. Placeholder-CIDR refusal (the CHANGE-ME
// documentation ranges) is deliberately NOT here: it is the renderer's
// render-time contract (fail, don't emit a stub), while a merely-declared
// placeholder must not stop the daemon from running local workflows.
func (c *Config) validateEgress() error {
	if c.Egress == nil {
		return nil
	}
	seen := make(map[string]bool, len(c.Egress.Allowlist))
	for i, group := range c.Egress.Allowlist {
		if err := group.validate(i, seen); err != nil {
			return err
		}
	}
	return nil
}

func (g EgressAllowlistGroup) validate(i int, seen map[string]bool) error {
	if g.Name == "" {
		return fmt.Errorf("egress.allowlist[%d]: name is required", i)
	}
	if !validSecretStoreName(g.Name) {
		return fmt.Errorf("egress.allowlist[%d]: name %q must be a lowercase DNS label (letters, digits, and interior hyphens, at most 63 characters)", i, g.Name)
	}
	if seen[g.Name] {
		return fmt.Errorf("egress.allowlist[%d] (%s): name is declared more than once", i, g.Name)
	}
	seen[g.Name] = true

	switch g.Kind {
	case EgressGroupKindProvider, EgressGroupKindModel, EgressGroupKindSandbox:
	default:
		return fmt.Errorf("egress.allowlist[%d] (%s): kind %q must be one of %q, %q, or %q",
			i, g.Name, g.Kind, EgressGroupKindProvider, EgressGroupKindModel, EgressGroupKindSandbox)
	}

	if len(g.CIDRs) == 0 {
		return fmt.Errorf("egress.allowlist[%d] (%s): cidrs must list at least one destination block", i, g.Name)
	}
	for j, cidr := range g.CIDRs {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			return fmt.Errorf("egress.allowlist[%d] (%s): cidrs[%d] %q is not a CIDR block: %w", i, g.Name, j, cidr, err)
		}
	}

	switch {
	case g.Source == "" && g.SourceSHA256 != "":
		return fmt.Errorf("egress.allowlist[%d] (%s): sourceSHA256 is set with no source — the marker names the hash OF the source document", i, g.Name)
	case g.Source != "":
		parsed, err := url.Parse(g.Source)
		if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
			return fmt.Errorf("egress.allowlist[%d] (%s): source %q must be an http(s) URL", i, g.Name, g.Source)
		}
		if g.SourceSHA256 == "" {
			return fmt.Errorf("egress.allowlist[%d] (%s): source is set with no sourceSHA256 — record the sha256 of the "+
				"document the CIDRs were transcribed from (e.g. `curl -s %s | shasum -a 256`) so drift is detectable",
				i, g.Name, g.Source)
		}
		if !sha256HexPattern.MatchString(g.SourceSHA256) {
			return fmt.Errorf("egress.allowlist[%d] (%s): sourceSHA256 %q must be 64 hex characters", i, g.Name, g.SourceSHA256)
		}
	}

	for j, port := range g.Ports {
		if port < 1 || port > 65535 {
			return fmt.Errorf("egress.allowlist[%d] (%s): ports[%d] %d is outside 1-65535", i, g.Name, j, port)
		}
	}
	return nil
}
