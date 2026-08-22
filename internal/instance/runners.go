package instance

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/goobers/goobers/internal/runnercap"
)

// Instance-config schema revisions (docs/design/dsl-3.0.md D8, decision record
// D3). schemaVersion is the config's first version field; absent means the
// pre-Goobernetes revision every existing install is on.
const (
	// InstanceSchemaVersionLegacy is the pre-Goobernetes instance schema — the
	// meaning of an absent schemaVersion field.
	InstanceSchemaVersionLegacy = 1
	// InstanceSchemaVersionRunners is the revision that introduces the
	// runners: inventory (the plural runner surface).
	InstanceSchemaVersionRunners = 2
)

// RunnerHostKind classifies a runner entry's host value. The kind set is
// designed extensible (decision record D3): adding a host kind is a new
// constant plus a new arm in ClassifyRunnerHost, never a schema break for the
// existing kinds.
type RunnerHostKind string

const (
	// RunnerHostSelf is the daemon's own host/pod — the only kind local
	// admission executes today, and the kind every legacy singular runner:
	// block resolves to.
	RunnerHostSelf RunnerHostKind = "self"
	// RunnerHostImage is a container image reference the dispatcher
	// instantiates one fresh pod per stage attempt from
	// (goobernetes-deployment-images.md §5).
	RunnerHostImage RunnerHostKind = "image"
	// RunnerHostDeployment names a consumer-authored Deployment used as a pod
	// template BY REFERENCE (decision record D3): the dispatcher instantiates
	// one fresh pod per stage attempt from its template — never resident
	// execution.
	RunnerHostDeployment RunnerHostKind = "deployment"
)

// RunnerHostSelfName is the literal host value naming the daemon host, and
// the name of the implicit entry a legacy singular runner: block maps to.
const RunnerHostSelfName = "self"

// RunnerEntry is one declared runner in the runners: inventory (decision
// record D3, dsl-3.0.md §3): a name, where stages placed on it execute
// (host), what it claims to provide, and the restriction effects it enforces.
// Claims are trusted in v1 — a false claim degrades to a runtime error with a
// named diagnostic, never a silent misroute.
type RunnerEntry struct {
	// Name identifies this runner in diagnostics, placement provenance, and
	// (later) queue keying. Lowercase DNS label, unique within the inventory.
	Name string `json:"name" yaml:"name"`
	// Host is where a stage placed on this runner executes: "self" (the
	// daemon host), a container image reference, or the name of a
	// consumer-authored Deployment whose pod template the dispatcher
	// instantiates per stage attempt. See ClassifyRunnerHost for how the
	// three kinds are told apart.
	Host string `json:"host" yaml:"host"`
	// Provides is this runner's claim set: OS, resource ceilings, and
	// toolchain/platform capabilities.
	Provides RunnerProvides `json:"provides,omitempty" yaml:"provides,omitempty"`
	// Restrictions are the isolation effects this runner ENFORCES (decision
	// record D7) — a stage requiring a restriction matches only runners that
	// enforce it. Drawn from the closed v1 effect list
	// (docs/design/goobernetes-restrictions.md §2).
	Restrictions []RunnerRestriction `json:"restrictions,omitempty" yaml:"restrictions,omitempty"`
}

// RunnerProvides is a runner's declared claim set. Quantities are ceilings
// (they become pod limits; stage minimums become requests — dsl-3.0.md D2),
// written as Kubernetes quantity strings verbatim.
type RunnerProvides struct {
	// OS is the runner's operating system: "linux", "windows", or "macOS"
	// (the dsl-3.0.md D2 enum). Empty claims no OS.
	OS RunnerOS `json:"os,omitempty" yaml:"os,omitempty"`
	// CPU is the CPU ceiling as a Kubernetes quantity string (e.g. "8000m").
	CPU string `json:"cpu,omitempty" yaml:"cpu,omitempty"`
	// Memory is the memory ceiling as a Kubernetes quantity string (e.g. "16Gi").
	Memory string `json:"memory,omitempty" yaml:"memory,omitempty"`
	// Disk is the disk ceiling as a Kubernetes quantity string (e.g. "100Gi").
	Disk string `json:"disk,omitempty" yaml:"disk,omitempty"`
	// Capabilities are the toolchain/platform capabilities this runner claims
	// are preinstalled — the same open vocabulary as the legacy
	// runner.capabilities (internal/runnercap), matched exactly.
	Capabilities []string `json:"capabilities,omitempty" yaml:"capabilities,omitempty"`
}

// RunnerOS is a runner's declared operating system — a validated enum, not a
// free token (dsl-3.0.md D2). The schema-enum registry guard
// (internal/workflow/v_current/schema_enum_registry_test.go) pins the
// published schema enum to exactly these consts.
type RunnerOS string

// The three schedulable operating systems of the runsOn/provides os enum.
const (
	RunnerOSLinux   RunnerOS = "linux"
	RunnerOSWindows RunnerOS = "windows"
	RunnerOSMacOS   RunnerOS = "macOS"
)

// RunnerRestriction is one isolation effect from the closed v1 effect list
// (decision record D7, docs/design/goobernetes-restrictions.md §2).
// Restrictions name effects, never mechanisms; growing this set is a product
// decision recorded there, not a config-side addition. The schema-enum
// registry guard pins the published schema enum to exactly these consts. The
// vocabulary itself lives in internal/runnercap (Restriction), the leaf both
// this inventory surface and the DSL 3.0 runsOn.restrictions surface consume,
// so the two closed lists cannot drift.
type RunnerRestriction string

// The closed v1 restriction effect list (goobernetes-restrictions.md §2).
// Spelled as literals — the v_current schema-enum registry guard parses these
// const literals to pin the published schema enum; TestRunnerRestrictionsMatchSharedVocabulary
// pins them to the runnercap vocabulary the validation map derives from.
const (
	RunnerRestrictionNetworkNone      RunnerRestriction = "network:none"
	RunnerRestrictionNetworkAllowlist RunnerRestriction = "network:allowlist"
	RunnerRestrictionFSReadonly       RunnerRestriction = "fs:readonly-except-workspace"
	RunnerRestrictionTmpEphemeral     RunnerRestriction = "tmp:ephemeral"
	RunnerRestrictionEnvDefaultDeny   RunnerRestriction = "env:default-deny"
)

// knownRunnerRestrictions is the closed-list membership check for validation,
// derived from the shared vocabulary.
var knownRunnerRestrictions = func() map[RunnerRestriction]bool {
	known := make(map[RunnerRestriction]bool)
	for _, r := range runnercap.KnownRestrictions() {
		known[RunnerRestriction(r)] = true
	}
	return known
}()

// KnownRunnerRestrictions returns the closed v1 restriction effect list,
// sorted for stable diagnostics.
func KnownRunnerRestrictions() []RunnerRestriction {
	out := make([]RunnerRestriction, 0, len(knownRunnerRestrictions))
	for r := range knownRunnerRestrictions {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// knownRunnerRestrictionNames renders the closed list for an error message.
func knownRunnerRestrictionNames() string {
	restrictions := KnownRunnerRestrictions()
	names := make([]string, len(restrictions))
	for i, r := range restrictions {
		names[i] = string(r)
	}
	return strings.Join(names, ", ")
}

// The image-reference grammar is a minimal reimplementation of the OCI
// distribution reference spelling (registry[:port]/repository[:tag][@digest],
// lowercase repository path). Only well-formedness is checked here — no
// registry is contacted, and claims are trusted in v1.
const (
	imageDomainComponent = `(?:[a-zA-Z0-9]|[a-zA-Z0-9][a-zA-Z0-9-]*[a-zA-Z0-9])`
	imageDomain          = imageDomainComponent + `(?:\.` + imageDomainComponent + `)*(?::[0-9]+)?`
	imagePathComponent   = `[a-z0-9]+(?:(?:[._]|__|[-]+)[a-z0-9]+)*`
	imageTag             = `[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}`
	imageDigest          = `[A-Za-z][A-Za-z0-9]*(?:[-_+.][A-Za-z][A-Za-z0-9]*)*:[0-9a-fA-F]{32,}`
)

var imageReferencePattern = regexp.MustCompile(
	`^(?:` + imageDomain + `/)?` + imagePathComponent + `(?:/` + imagePathComponent + `)*(?::` + imageTag + `)?(?:@` + imageDigest + `)?$`,
)

// dns1123SubdomainPattern is the Kubernetes object-name grammar a Deployment
// name must satisfy (RFC 1123 subdomain).
var dns1123SubdomainPattern = regexp.MustCompile(
	`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]*[a-z0-9])?)*$`,
)

// ClassifyRunnerHost decides which host kind a runner entry's host value
// names and validates its spelling. The rule: the literal "self" is the
// daemon host; a value carrying an image-reference marker ("/", ":", or "@")
// must be a well-formed image reference; any other bare name must be a
// well-formed Deployment name (RFC 1123 subdomain, at most 253 characters).
// A plain image shorthand like "ubuntu" is therefore read as a Deployment
// name — an image host always spells at least a registry, tag, or digest.
func ClassifyRunnerHost(host string) (RunnerHostKind, error) {
	switch {
	case host == "":
		return "", fmt.Errorf("host is required (%q, an image reference, or a Deployment name)", RunnerHostSelfName)
	case host == RunnerHostSelfName:
		return RunnerHostSelf, nil
	case strings.ContainsAny(host, "/:@"):
		if !imageReferencePattern.MatchString(host) {
			return "", fmt.Errorf("host %q is not a valid image reference (registry[:port]/repository[:tag][@digest], lowercase repository path)", host)
		}
		return RunnerHostImage, nil
	default:
		if len(host) > 253 || !dns1123SubdomainPattern.MatchString(host) {
			return "", fmt.Errorf("host %q is not %q, an image reference, or a Deployment name (a lowercase RFC 1123 subdomain of at most 253 characters)", host, RunnerHostSelfName)
		}
		return RunnerHostDeployment, nil
	}
}

// EffectiveSchemaVersion resolves the instance schema revision: an absent
// schemaVersion means the pre-Goobernetes schema (dsl-3.0.md D8).
func (c *Config) EffectiveSchemaVersion() int {
	if c.SchemaVersion == nil {
		return InstanceSchemaVersionLegacy
	}
	return *c.SchemaVersion
}

// ResolvedRunners returns the effective runner inventory: the declared
// runners: list when one is present, otherwise the implicit "self" entry
// synthesized from the legacy singular runner: block — the zero-change
// upgrade every existing install rides (decision record D3). Only the self
// entry executes anything today; remote host kinds are declared inventory the
// admission/dispatch work (#3506 onward) consumes.
func (c *Config) ResolvedRunners() []RunnerEntry {
	if len(c.Runners) > 0 {
		return c.Runners
	}
	return []RunnerEntry{{
		Name:     RunnerHostSelfName,
		Host:     RunnerHostSelfName,
		Provides: RunnerProvides{Capabilities: c.Runner.Capabilities},
	}}
}

// SelfRunnerCapabilities returns the capability claims of the resolved
// inventory's self entry — on an instance with no runners: list this is the
// legacy runner.capabilities slice itself, so the startup cross-check
// (CheckCapabilityRequirements) and the scheduler admit path read claims
// byte-identical to every previous release. With a declared inventory it is
// the first host:"self" entry's provides.capabilities; an inventory with no
// self entry claims nothing locally.
func (c *Config) SelfRunnerCapabilities() []string {
	if len(c.Runners) == 0 {
		return c.Runner.Capabilities
	}
	for i := range c.Runners {
		if c.Runners[i].Host == RunnerHostSelfName {
			return c.Runners[i].Provides.Capabilities
		}
	}
	return nil
}

// validateSchemaVersion accepts the two known revisions. Only an ABSENT
// schemaVersion means legacy; an explicit 0 is refused, matching the
// published schema's enum [1, 2] (the pointer field is what lets the strict
// loader tell the two apart).
func (c *Config) validateSchemaVersion() error {
	if c.SchemaVersion == nil {
		return nil
	}
	switch *c.SchemaVersion {
	case InstanceSchemaVersionLegacy, InstanceSchemaVersionRunners:
		return nil
	default:
		return fmt.Errorf("schemaVersion %d is not supported (supported: %d and %d; absent means %d)",
			*c.SchemaVersion, InstanceSchemaVersionLegacy, InstanceSchemaVersionRunners, InstanceSchemaVersionLegacy)
	}
}

// validateRunners checks the runners: inventory fail-first at load, like every
// other instance.yaml section. It also enforces the supersession rule
// (decision record D3, dsl-3.0.md §3): a declared inventory owns capability
// claims, so it cannot coexist with legacy runner.capabilities — while the
// legacy block's execution settings (envPassthrough, timeouts, harnessCommand)
// keep their current homes and stay valid alongside runners:.
func (c *Config) validateRunners() error {
	if len(c.Runners) == 0 {
		return nil
	}
	if len(c.Runner.Capabilities) > 0 {
		return fmt.Errorf("runner.capabilities and runners: cannot both be declared — runners: supersedes the " +
			"singular runner block's capability claims (Goobernetes decision D3): move the claims into the " +
			"runners: entry with host \"self\" (provides.capabilities) and remove runner.capabilities; other " +
			"runner settings (envPassthrough, livenessTimeout, defaultStageTimeout, harnessCommand) keep their current home")
	}
	seen := make(map[string]bool, len(c.Runners))
	for i := range c.Runners {
		// EngineProjectionEnabled is the daemon's own connection-configured
		// predicate (yaml engine: block, or a hostPort env override). Gating
		// on c.Engine != nil instead would also pass a namespace-only or
		// task-queue-only env override — LoadConfig synthesizes cfg.Engine on
		// ANY override — letting a remote runner load that can never dispatch.
		if err := c.Runners[i].validate(i, seen, c.EngineProjectionEnabled()); err != nil {
			return err
		}
	}
	return nil
}

func (r RunnerEntry) validate(i int, seen map[string]bool, engineConfigured bool) error {
	if r.Name == "" {
		return fmt.Errorf("runners[%d]: name is required", i)
	}
	// The same lowercase DNS-label grammar as secret-store names: runner
	// names flow into diagnostics now and queue/pod naming later (decision
	// record D9), so anything Kubernetes could not name is refused up front.
	if !validSecretStoreName(r.Name) {
		return fmt.Errorf("runners[%d]: name %q must be a lowercase DNS label (letters, digits, and interior hyphens, at most 63 characters)", i, r.Name)
	}
	if seen[r.Name] {
		return fmt.Errorf("runners[%d] (%s): name is declared more than once", i, r.Name)
	}
	seen[r.Name] = true

	kind, err := ClassifyRunnerHost(r.Host)
	if err != nil {
		return fmt.Errorf("runners[%d] (%s): %w", i, r.Name, err)
	}
	// A non-self runner dispatches through the engine connection, so an
	// inventory that declares one without engine: config can never execute a
	// stage on it. This is the condition the coded RNR002 validation surfaces
	// (dsl-3.0.md §5, arriving with the constraint-solve work); until then it
	// fails first here like every other malformed-instance.yaml error.
	if kind != RunnerHostSelf && !engineConfigured {
		return fmt.Errorf("runners[%d] (%s): host %q is not %q and the instance declares no engine: block — "+
			"a remote runner dispatches through the engine connection (engine.hostPort/namespace/taskQueue), "+
			"so declare engine: or make this runner host %q", i, r.Name, r.Host, RunnerHostSelfName, RunnerHostSelfName)
	}
	if err := r.Provides.validate(i, r.Name); err != nil {
		return err
	}
	return r.validateRestrictions(i)
}

func (p RunnerProvides) validate(i int, name string) error {
	switch p.OS {
	case "", RunnerOSLinux, RunnerOSWindows, RunnerOSMacOS:
	default:
		return fmt.Errorf("runners[%d] (%s): provides.os %q must be one of %q, %q, or %q",
			i, name, p.OS, RunnerOSLinux, RunnerOSWindows, RunnerOSMacOS)
	}
	for _, quantity := range []struct {
		field string
		value string
	}{
		{field: "cpu", value: p.CPU},
		{field: "memory", value: p.Memory},
		{field: "disk", value: p.Disk},
	} {
		if quantity.value == "" {
			continue
		}
		parsed, err := resource.ParseQuantity(quantity.value)
		if err != nil {
			return fmt.Errorf("runners[%d] (%s): provides.%s %q must be a Kubernetes quantity string (for example \"2000m\", \"4Gi\"): %w",
				i, name, quantity.field, quantity.value, err)
		}
		if parsed.Sign() <= 0 {
			return fmt.Errorf("runners[%d] (%s): provides.%s must be positive, got %q", i, name, quantity.field, quantity.value)
		}
	}
	for j, token := range p.Capabilities {
		if err := runnercap.ValidateToken(token); err != nil {
			return fmt.Errorf("runners[%d] (%s): provides.capabilities[%d]: %w", i, name, j, err)
		}
	}
	return nil
}

func (r RunnerEntry) validateRestrictions(i int) error {
	seen := make(map[RunnerRestriction]bool, len(r.Restrictions))
	for j, restriction := range r.Restrictions {
		if !knownRunnerRestrictions[restriction] {
			return fmt.Errorf("runners[%d] (%s): restrictions[%d]: unknown restriction %q (the closed v1 effect list: %s)",
				i, r.Name, j, restriction, knownRunnerRestrictionNames())
		}
		if seen[restriction] {
			return fmt.Errorf("runners[%d] (%s): restrictions[%d]: %q is declared more than once", i, r.Name, j, restriction)
		}
		seen[restriction] = true
	}
	return nil
}
