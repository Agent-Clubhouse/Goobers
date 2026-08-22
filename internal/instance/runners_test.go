package instance

import (
	"reflect"
	"strings"
	"testing"
)

// legacyRunnerBody is the shape every pre-Goobernetes install is on: a
// singular runner: block and no schemaVersion. The zero-change upgrade
// (decision record D3) is measured against exactly this.
const legacyRunnerBody = `
apiVersion: goobers.dev/v1alpha1
kind: Instance
repos:
  - provider: github
    owner: acme
    name: web
    token:
      env: GITHUB_TOKEN
runner:
  capabilities: [dotnet@8, os=windows]
`

func TestLegacyRunnerBlockMapsToImplicitSelfEntry(t *testing.T) {
	cfg, err := LoadConfig(writeInstanceYAML(t, legacyRunnerBody))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.EffectiveSchemaVersion(); got != InstanceSchemaVersionLegacy {
		t.Errorf("EffectiveSchemaVersion() = %d, want %d", got, InstanceSchemaVersionLegacy)
	}
	want := []RunnerEntry{{
		Name:     RunnerHostSelfName,
		Host:     RunnerHostSelfName,
		Provides: RunnerProvides{Capabilities: []string{"dotnet@8", "os=windows"}},
	}}
	if got := cfg.ResolvedRunners(); !reflect.DeepEqual(got, want) {
		t.Errorf("ResolvedRunners() = %+v, want %+v", got, want)
	}
	// The admit-path consumers must read the very slice the legacy block
	// declared — identity, not just equality, is what makes the upgrade
	// byte-identical for every existing consumer.
	if got := cfg.SelfRunnerCapabilities(); len(got) != 2 || &got[0] != &cfg.Runner.Capabilities[0] {
		t.Errorf("SelfRunnerCapabilities() = %v, want the legacy runner.capabilities slice itself", got)
	}
}

func TestLegacyConfigRoundTripsWithoutNewFields(t *testing.T) {
	cfg, err := LoadConfig(writeInstanceYAML(t, legacyRunnerBody))
	if err != nil {
		t.Fatal(err)
	}
	written, err := marshalConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"schemaVersion", "runners"} {
		if strings.Contains(string(written), field) {
			t.Errorf("written legacy config carries %q, breaking the zero-change upgrade:\n%s", field, written)
		}
	}
}

func TestEmptyConfigResolvesToImplicitSelfEntry(t *testing.T) {
	cfg, err := LoadConfig(writeInstanceYAML(t, `
apiVersion: goobers.dev/v1alpha1
kind: Instance
repos: []
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.EffectiveSchemaVersion(); got != InstanceSchemaVersionLegacy {
		t.Errorf("EffectiveSchemaVersion() = %d, want %d", got, InstanceSchemaVersionLegacy)
	}
	want := []RunnerEntry{{Name: RunnerHostSelfName, Host: RunnerHostSelfName}}
	if got := cfg.ResolvedRunners(); !reflect.DeepEqual(got, want) {
		t.Errorf("ResolvedRunners() = %+v, want %+v", got, want)
	}
	if got := cfg.SelfRunnerCapabilities(); got != nil {
		t.Errorf("SelfRunnerCapabilities() = %v, want nil", got)
	}
}

func TestLoadConfigSchemaVersion(t *testing.T) {
	tests := []struct {
		name    string
		version string
		want    int
		wantErr string
	}{
		{name: "legacy accepted explicitly", version: "schemaVersion: 1", want: 1},
		{name: "runners revision accepted", version: "schemaVersion: 2", want: 2},
		{name: "future revision refused", version: "schemaVersion: 3", wantErr: "schemaVersion 3 is not supported"},
		{name: "negative revision refused", version: "schemaVersion: -1", wantErr: "schemaVersion -1 is not supported"},
		// Only an ABSENT schemaVersion means legacy — the published schema's
		// enum is [1, 2], and its error text says so. An explicit 0 slipping
		// through as legacy would make the loader accept what the contract
		// (goobers schema instance, editor validation) refuses.
		{name: "explicit zero refused", version: "schemaVersion: 0", wantErr: "schemaVersion 0 is not supported"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := LoadConfig(writeInstanceYAML(t, `
apiVersion: goobers.dev/v1alpha1
kind: Instance
`+test.version+`
repos: []
`))
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("error = %v, want mention of %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := cfg.EffectiveSchemaVersion(); got != test.want {
				t.Errorf("EffectiveSchemaVersion() = %d, want %d", got, test.want)
			}
		})
	}
}

func TestLoadConfigDeclaredRunnersInventory(t *testing.T) {
	cfg, err := LoadConfig(writeInstanceYAML(t, `
apiVersion: goobers.dev/v1alpha1
kind: Instance
schemaVersion: 2
repos: []
runners:
  - name: self
    host: self
    provides:
      os: linux
      cpu: 8000m
      memory: 16Gi
      disk: 100Gi
      capabilities: [go@1.26, make]
  - name: ci-linux
    host: ghcr.io/example/goobers-ci:v0.7.0
    provides:
      os: linux
      cpu: 4000m
      memory: 8Gi
      disk: 60Gi
      capabilities: [go@1.26]
    restrictions: [network:allowlist, tmp:ephemeral]
  - name: win-pool
    host: win-runner-pool
    provides:
      os: windows
engine:
  hostPort: temporal.goobers-system:7233
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.EffectiveSchemaVersion(); got != InstanceSchemaVersionRunners {
		t.Errorf("EffectiveSchemaVersion() = %d, want %d", got, InstanceSchemaVersionRunners)
	}
	resolved := cfg.ResolvedRunners()
	if len(resolved) != 3 || resolved[1].Name != "ci-linux" {
		t.Fatalf("ResolvedRunners() = %+v, want the 3 declared entries", resolved)
	}
	if got, want := cfg.SelfRunnerCapabilities(), []string{"go@1.26", "make"}; !reflect.DeepEqual(got, want) {
		t.Errorf("SelfRunnerCapabilities() = %v, want %v", got, want)
	}
}

func TestLoadConfigRemoteRunnerAcceptsEngineFromEnvironment(t *testing.T) {
	// "No resolvable engine:" means neither instance.yaml nor the environment
	// overrides configured a connection; an env-provided hostPort satisfies
	// the same condition the engine: block does.
	t.Setenv(TemporalHostPortEnv, "temporal.internal:7233")
	_, err := LoadConfig(writeInstanceYAML(t, `
apiVersion: goobers.dev/v1alpha1
kind: Instance
repos: []
runners:
  - name: ci-linux
    host: ghcr.io/example/goobers-ci:v0.7.0
`))
	if err != nil {
		t.Fatalf("remote runner with env-resolved engine was refused: %v", err)
	}
}

func TestLoadConfigRemoteRunnerRefusedWithNonConnectionEngineEnvOnly(t *testing.T) {
	// LoadConfig synthesizes cfg.Engine on ANY engine env override, but the
	// daemon's connection-configured predicate (EngineProjectionEnabled)
	// requires a yaml engine: block or a hostPort override. A namespace-only
	// or task-queue-only override must therefore NOT satisfy the remote-runner
	// check: a remote runner accepted on the synthesized block alone loads
	// cleanly and can never dispatch.
	for _, test := range []struct {
		name string
		env  string
	}{
		{name: "task-queue override only", env: TaskQueueEnv},
		{name: "namespace override only", env: TemporalNamespaceEnv},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv(test.env, "goobers-somewhere")
			_, err := LoadConfig(writeInstanceYAML(t, `
apiVersion: goobers.dev/v1alpha1
kind: Instance
repos: []
runners:
  - name: ci-linux
    host: ghcr.io/example/goobers-ci:v0.7.0
`))
			if err == nil || !strings.Contains(err.Error(), "declares no engine") {
				t.Fatalf("remote runner with a non-connection engine env override loaded without error (err = %v), want the declares-no-engine refusal", err)
			}
		})
	}
}

func TestRunnersSupersedeLegacyCapabilityClaims(t *testing.T) {
	_, err := LoadConfig(writeInstanceYAML(t, `
apiVersion: goobers.dev/v1alpha1
kind: Instance
repos: []
runner:
  capabilities: [dotnet@8]
runners:
  - name: self
    host: self
`))
	if err == nil || !strings.Contains(err.Error(), "runners: supersedes") {
		t.Fatalf("coexisting runner.capabilities and runners: was not refused with the migration named: %v", err)
	}
}

func TestRunnersCoexistWithLegacyExecutionSettings(t *testing.T) {
	// envPassthrough/timeout/harnessCommand keep their current homes
	// (dsl-3.0.md §3): only the capability claims are superseded.
	cfg, err := LoadConfig(writeInstanceYAML(t, `
apiVersion: goobers.dev/v1alpha1
kind: Instance
repos: []
runner:
  envPassthrough: [NUGET_CONFIG_FILE]
  defaultStageTimeout: 25m
runners:
  - name: self
    host: self
    provides:
      capabilities: [dotnet@8]
`))
	if err != nil {
		t.Fatalf("legacy execution settings alongside runners: were refused: %v", err)
	}
	if got, want := cfg.SelfRunnerCapabilities(), []string{"dotnet@8"}; !reflect.DeepEqual(got, want) {
		t.Errorf("SelfRunnerCapabilities() = %v, want %v", got, want)
	}
}

func validRunnersBase() *Config {
	return &Config{
		APIVersion: ConfigAPIVersion,
		Kind:       ConfigKind,
		Engine:     &EngineConfig{HostPort: "temporal.internal:7233", Namespace: "default", TaskQueue: "goobers-engine"},
	}
}

func TestValidateRunnersEntryBranches(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{
			name: "name is required",
			mutate: func(c *Config) {
				c.Runners = []RunnerEntry{{Host: "self"}}
			},
			wantErr: "runners[0]: name is required",
		},
		{
			name: "name must be a lowercase DNS label",
			mutate: func(c *Config) {
				c.Runners = []RunnerEntry{{Name: "CI", Host: "self"}}
			},
			wantErr: "must be a lowercase DNS label",
		},
		{
			name: "duplicate names are refused",
			mutate: func(c *Config) {
				c.Runners = []RunnerEntry{
					{Name: "self", Host: "self"},
					{Name: "self", Host: "self"},
				}
			},
			wantErr: "declared more than once",
		},
		{
			name: "host is required",
			mutate: func(c *Config) {
				c.Runners = []RunnerEntry{{Name: "a"}}
			},
			wantErr: "host is required",
		},
		{
			name: "malformed image reference is refused",
			mutate: func(c *Config) {
				c.Runners = []RunnerEntry{{Name: "a", Host: "ghcr.io/Example/repo:v1"}}
			},
			wantErr: "not a valid image reference",
		},
		{
			name: "malformed deployment name is refused",
			mutate: func(c *Config) {
				c.Runners = []RunnerEntry{{Name: "a", Host: "My_Deployment"}}
			},
			wantErr: "Deployment name",
		},
		{
			name: "image host without engine is refused",
			mutate: func(c *Config) {
				c.Engine = nil
				c.Runners = []RunnerEntry{{Name: "a", Host: "ghcr.io/example/ci:v1"}}
			},
			wantErr: "declares no engine",
		},
		{
			name: "deployment host without engine is refused",
			mutate: func(c *Config) {
				c.Engine = nil
				c.Runners = []RunnerEntry{{Name: "a", Host: "runner-pool"}}
			},
			wantErr: "declares no engine",
		},
		{
			name: "self host needs no engine",
			mutate: func(c *Config) {
				c.Engine = nil
				c.Runners = []RunnerEntry{{Name: "self", Host: "self"}}
			},
		},
		{
			name: "unknown provides.os is refused",
			mutate: func(c *Config) {
				c.Runners = []RunnerEntry{{Name: "a", Host: "self", Provides: RunnerProvides{OS: "darwin"}}}
			},
			wantErr: `provides.os "darwin"`,
		},
		{
			name: "the three OS enum values are accepted",
			mutate: func(c *Config) {
				c.Runners = []RunnerEntry{
					{Name: "a", Host: "self", Provides: RunnerProvides{OS: RunnerOSLinux}},
					{Name: "b", Host: "runner-b", Provides: RunnerProvides{OS: RunnerOSWindows}},
					{Name: "c", Host: "runner-c", Provides: RunnerProvides{OS: RunnerOSMacOS}},
				}
			},
		},
		{
			name: "unparseable cpu quantity is refused",
			mutate: func(c *Config) {
				c.Runners = []RunnerEntry{{Name: "a", Host: "self", Provides: RunnerProvides{CPU: "fast"}}}
			},
			wantErr: `provides.cpu "fast"`,
		},
		{
			name: "zero memory ceiling is refused",
			mutate: func(c *Config) {
				c.Runners = []RunnerEntry{{Name: "a", Host: "self", Provides: RunnerProvides{Memory: "0"}}}
			},
			wantErr: "provides.memory must be positive",
		},
		{
			name: "negative disk ceiling is refused",
			mutate: func(c *Config) {
				c.Runners = []RunnerEntry{{Name: "a", Host: "self", Provides: RunnerProvides{Disk: "-1Gi"}}}
			},
			wantErr: "provides.disk must be positive",
		},
		{
			name: "quantities parse with Kubernetes semantics",
			mutate: func(c *Config) {
				c.Runners = []RunnerEntry{{Name: "a", Host: "self", Provides: RunnerProvides{CPU: "2000m", Memory: "4Gi", Disk: "20Gi"}}}
			},
		},
		{
			name: "malformed capability token is refused",
			mutate: func(c *Config) {
				c.Runners = []RunnerEntry{{Name: "a", Host: "self", Provides: RunnerProvides{Capabilities: []string{"has space"}}}}
			},
			wantErr: "provides.capabilities[0]",
		},
		{
			name: "unknown restriction is refused naming the closed list",
			mutate: func(c *Config) {
				c.Runners = []RunnerEntry{{Name: "a", Host: "self", Restrictions: []RunnerRestriction{"network:proxy"}}}
			},
			wantErr: "closed v1 effect list",
		},
		{
			name: "duplicate restriction is refused",
			mutate: func(c *Config) {
				c.Runners = []RunnerEntry{{Name: "a", Host: "self", Restrictions: []RunnerRestriction{"tmp:ephemeral", "tmp:ephemeral"}}}
			},
			wantErr: "declared more than once",
		},
		{
			name: "every closed-list restriction is accepted",
			mutate: func(c *Config) {
				c.Runners = []RunnerEntry{{Name: "a", Host: "self", Restrictions: KnownRunnerRestrictions()}}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := validRunnersBase()
			test.mutate(cfg)
			err := cfg.Validate()
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Validate() = %v, want mention of %q", err, test.wantErr)
			}
		})
	}
}

func TestClassifyRunnerHost(t *testing.T) {
	tests := []struct {
		host    string
		want    RunnerHostKind
		wantErr bool
	}{
		{host: "self", want: RunnerHostSelf},
		{host: "ghcr.io/example/goobers-ci:v0.7.0", want: RunnerHostImage},
		{host: "alpine:3.18", want: RunnerHostImage},
		{host: "localhost:5000/team/runner", want: RunnerHostImage},
		{host: "ghcr.io/example/ci@sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08", want: RunnerHostImage},
		{host: "win-runner-pool", want: RunnerHostDeployment},
		{host: "pool.goobers-system", want: RunnerHostDeployment},
		{host: "", wantErr: true},
		{host: "ghcr.io/Example/repo:v1", wantErr: true},
		{host: "registry/repo//broken", wantErr: true},
		{host: "My_Deployment", wantErr: true},
		{host: "-leading-hyphen", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.host, func(t *testing.T) {
			kind, err := ClassifyRunnerHost(test.host)
			if test.wantErr {
				if err == nil {
					t.Fatalf("ClassifyRunnerHost(%q) = %q, want error", test.host, kind)
				}
				return
			}
			if err != nil {
				t.Fatalf("ClassifyRunnerHost(%q): %v", test.host, err)
			}
			if kind != test.want {
				t.Errorf("ClassifyRunnerHost(%q) = %q, want %q", test.host, kind, test.want)
			}
		})
	}
}

func TestSelfRunnerCapabilitiesWithoutSelfEntryClaimsNothing(t *testing.T) {
	cfg := validRunnersBase()
	cfg.Runners = []RunnerEntry{{
		Name:     "ci-linux",
		Host:     "ghcr.io/example/ci:v1",
		Provides: RunnerProvides{Capabilities: []string{"go@1.26"}},
	}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := cfg.SelfRunnerCapabilities(); got != nil {
		t.Errorf("SelfRunnerCapabilities() = %v, want nil for an inventory with no self entry", got)
	}
}
