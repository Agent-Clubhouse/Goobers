package instance

import (
	"strings"
	"testing"
)

func validEgressConfig() *EgressConfig {
	return &EgressConfig{Allowlist: []EgressAllowlistGroup{
		{
			Name: "github-provider", Kind: EgressGroupKindProvider,
			Source: "https://api.github.com/meta", SourceSHA256: strings.Repeat("ab", 32),
			CIDRs: []string{"140.82.112.0/20"}, Ports: []int{443},
		},
		{Name: "sandbox-local", Kind: EgressGroupKindSandbox, CIDRs: []string{"10.20.0.0/16"}},
	}}
}

func TestValidateEgressAcceptsWellFormedConfig(t *testing.T) {
	cfg := &Config{Egress: validEgressConfig()}
	if err := cfg.validateEgress(); err != nil {
		t.Fatalf("well-formed egress config refused: %v", err)
	}
	if err := (&Config{}).validateEgress(); err != nil {
		t.Fatalf("nil egress config refused: %v", err)
	}
}

func TestValidateEgressFailFirst(t *testing.T) {
	mutate := func(f func(*EgressConfig)) *Config {
		cfg := &Config{Egress: validEgressConfig()}
		f(cfg.Egress)
		return cfg
	}
	cases := []struct {
		name    string
		cfg     *Config
		wantErr string
	}{
		{"missing name", mutate(func(e *EgressConfig) { e.Allowlist[0].Name = "" }), "name is required"},
		{"bad name", mutate(func(e *EgressConfig) { e.Allowlist[0].Name = "Not_A_Label" }), "lowercase DNS label"},
		{"duplicate name", mutate(func(e *EgressConfig) { e.Allowlist[1].Name = "github-provider" }), "more than once"},
		{"unknown kind", mutate(func(e *EgressConfig) { e.Allowlist[0].Kind = "everything" }), "kind"},
		{"no cidrs", mutate(func(e *EgressConfig) { e.Allowlist[0].CIDRs = nil }), "at least one"},
		{"malformed cidr", mutate(func(e *EgressConfig) { e.Allowlist[0].CIDRs = []string{"CHANGE-ME"} }), "not a CIDR"},
		{"source without hash", mutate(func(e *EgressConfig) { e.Allowlist[0].SourceSHA256 = "" }), "no sourceSHA256"},
		{"hash without source", mutate(func(e *EgressConfig) {
			e.Allowlist[1].SourceSHA256 = strings.Repeat("ab", 32)
		}), "no source"},
		{"short hash", mutate(func(e *EgressConfig) { e.Allowlist[0].SourceSHA256 = "abc123" }), "64 hex"},
		{"non-http source", mutate(func(e *EgressConfig) { e.Allowlist[0].Source = "ftp://example.com/meta" }), "http(s)"},
		{"port out of range", mutate(func(e *EgressConfig) { e.Allowlist[0].Ports = []int{0} }), "1-65535"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.validateEgress()
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want mention of %q", err, tc.wantErr)
			}
		})
	}
}
