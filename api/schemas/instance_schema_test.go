package schemas

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v5"
	"sigs.k8s.io/yaml"
)

// instance.yaml was the one major object with no published schema and no
// explain surface (cold-start 2026-08-08: `goobers schema instance` ->
// unknown kind, `goobers explain instance.repos` -> unknown selector), even
// though `goobers init` names it as the first file to edit. These tests pin
// the two halves of that contract: the published schema accepts every shape
// the product itself writes or documents, and it refuses the structural
// mistakes the cold-start walkthroughs actually made.

// compileInstanceSchema compiles the embedded instance contract with every
// sibling schema registered, so cross-file $refs (the repository execution
// settings shared with instance-repository-execution.schema.json) resolve.
func compileInstanceSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	compiler := jsonschema.NewCompiler()
	compiler.Draft = jsonschema.Draft2020
	for _, file := range Files() {
		raw, err := FS.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		if err := compiler.AddResource(BaseURI+file, bytes.NewReader(raw)); err != nil {
			t.Fatalf("add %s: %v", file, err)
		}
	}
	compiled, err := compiler.Compile(BaseURI + "instance.schema.json")
	if err != nil {
		t.Fatalf("compile instance schema: %v", err)
	}
	return compiled
}

func validateInstanceYAML(t *testing.T, schema *jsonschema.Schema, document string) error {
	t.Helper()
	raw, err := yaml.YAMLToJSON([]byte(document))
	if err != nil {
		t.Fatalf("convert YAML: %v", err)
	}
	var value any
	if err := yaml.Unmarshal(raw, &value); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	return schema.Validate(value)
}

func TestInstanceSchemaAcceptsEveryShippedInstanceShape(t *testing.T) {
	schema := compileInstanceSchema(t)

	// The template operators are told to copy verbatim is the de facto spec:
	// if the published schema rejected it, the schema would be the bug.
	reference, err := os.ReadFile(filepath.Join("..", "..", "reference-workflows", "instance.yaml.example"))
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		document string
	}{
		{"reference template", string(reference)},
		{"github pat minimal", `
apiVersion: goobers.dev/v1alpha1
kind: Instance
repos:
  - provider: github
    owner: acme
    name: web
    token:
      env: GOOBERS_GITHUB_TOKEN
`},
		{"github cli selected identity", `
apiVersion: goobers.dev/v1alpha1
kind: Instance
selfIdentity: octocat
repos:
  - provider: github
    owner: acme
    name: web
    token:
      githubCLI:
        hostname: github.com
        user: octocat
`},
		{"temporal engine", `
apiVersion: goobers.dev/v1alpha1
kind: Instance
repos: []
engine:
  hostPort: temporal.internal:7233
  namespace: production
  taskQueue: goobers-engine
`},
		{"ado three part identity", `
apiVersion: goobers.dev/v1alpha1
kind: Instance
repos:
  - provider: ado
    owner: contoso
    project: Payments
    name: payments-api
    auth:
      kind: workload-identity
`},
		{"gitea self hosted", `
apiVersion: goobers.dev/v1alpha1
kind: Instance
repos:
  - provider: gitea
    baseUrl: https://gitea.example.com
    owner: acme
    name: web
    token:
      file: /etc/goobers/gitea-token
`},
		{"github app minted identity", `
apiVersion: goobers.dev/v1alpha1
kind: Instance
repos:
  - provider: github
    owner: acme
    name: web
    auth:
      kind: github-app
      appId: 123456
      installationId: "78901234"
      privateKey:
        file: /etc/goobers/app.pem
`},
		{"runner claims and execution defaults", `
apiVersion: goobers.dev/v1alpha1
kind: Instance
repos:
  - provider: github
    owner: acme
    name: web
    token:
      env: GOOBERS_GITHUB_TOKEN
    largeRepo: true
    workspace:
      pinned: true
      cleanPolicy: ignored-safe
    pathLength:
      maxPathLength: 260
      buildOutputAllowance: 60
    defaultStageTimeout: 4h
    runControls:
      maxRepasses: 3
      stalledRunTimeout: 6h
      maxRunDuration: 24h
runner:
  capabilities: [dotnet@8, os=windows]
  envPassthrough: [NUGET_CONFIG_FILE, MSBUILDDISABLENODEREUSE]
  defaultStageTimeout: 25m
  harnessCommand:
    copilot: [agency, copilot]
runConditions:
  maxParallelRuns: 2
  workflowBudgets:
    implementation: 4
  claimsLockTimeout: 30s
`},
		{"schemaVersion 2 runners inventory", `
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
      capabilities: [go@1.26, make, gcc]
  - name: ci-linux
    host: ghcr.io/example/goobers-ci:v0.7.0
    provides:
      os: linux
      cpu: 4000m
      memory: 8Gi
      disk: 60Gi
      capabilities: [go@1.26]
      shell: true
      harnesses: [copilot, claude]
    restrictions: [network:allowlist, tmp:ephemeral]
  - name: win-pool
    host: win-runner-pool
    provides:
      os: windows
engine:
  hostPort: temporal.goobers-system:7233
`},
		{"credentials stores and telemetry", `
apiVersion: goobers.dev/v1alpha1
kind: Instance
repos:
  - provider: github
    owner: acme
    name: web
    token:
      store: prod-kv/github-token
secretStores:
  - name: prod-kv
    kind: azure-key-vault
    vaultURI: https://acme.vault.azure.net
    auth:
      kind: workload-identity
    cacheTTLSeconds: 300
credentials:
  - capability: github:pr:review
    token:
      env: GOOBERS_GITHUB_REVIEW_TOKEN
  - mcp: acme-docs
    token:
      env: GOOBERS_ACME_DOCS_TOKEN
telemetry:
  enabled: true
  otlp:
    endpoint: http://127.0.0.1:4317
    insecure: true
  retention:
    enabled: true
    window: 90d
    maxRuns: 500
sandbox:
  agentic: enforced
workcopies:
  partialClone: true
`},
		{"telemetry otlp tls trusted collector (#3804)", `
apiVersion: goobers.dev/v1alpha1
kind: Instance
repos: []
telemetry:
  enabled: true
  otlp:
    endpoint: goobers-collector.goobers-system.svc.cluster.local:4317
    tls:
      caFile: /etc/goobers/otlp-ca.crt
      serverName: goobers-collector.goobers-system.svc.cluster.local
      certFile: /etc/goobers/otlp-client.crt
      keyFile: /etc/goobers/otlp-client.key
`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateInstanceYAML(t, schema, test.document); err != nil {
				t.Fatalf("valid instance.yaml was rejected: %v", err)
			}
		})
	}
}

// TestInstanceSchemaAcceptsWorkflowSourceGitHubAppFixture is #3274's agreed
// acceptance: a sanitized copy of the cloud deployment's real instance.yaml,
// exercising the three-way combination a synthetic document misses — per-repo
// App auth x daemonIdentity App auth x workflowSource App auth, with two
// different installation IDs. Before $defs.workflowSource gained the auth
// property this failed with exactly one error ('auth' unexpected under
// workflowSource, because the def is additionalProperties:false), so the
// deployment's own config could not validate. It must now validate with zero
// errors, unchanged.
func TestInstanceSchemaAcceptsWorkflowSourceGitHubAppFixture(t *testing.T) {
	schema := compileInstanceSchema(t)
	fixture, err := os.ReadFile(filepath.Join("testdata", "instance-workflowsource-app-auth.fixture.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := validateInstanceYAML(t, schema, string(fixture)); err != nil {
		t.Fatalf("#3274 acceptance fixture was rejected: %v", err)
	}
}

func TestInstanceSchemaRefusesStructurallyImpossibleInstances(t *testing.T) {
	schema := compileInstanceSchema(t)

	tests := []struct {
		name     string
		document string
		want     string
	}{
		{"engine hostPort without port", `
apiVersion: goobers.dev/v1alpha1
kind: Instance
repos: []
engine:
  hostPort: temporal.internal
`, "hostPort"},
		{"ado repo without project", `
apiVersion: goobers.dev/v1alpha1
kind: Instance
repos:
  - provider: ado
    owner: contoso
    name: payments-api
    token:
      env: GOOBERS_ADO_TOKEN
`, "project"},
		{"gitea repo without baseUrl", `
apiVersion: goobers.dev/v1alpha1
kind: Instance
repos:
  - provider: gitea
    owner: acme
    name: web
    token:
      env: GOOBERS_GITEA_TOKEN
`, "baseUrl"},
		{"unknown provider", `
apiVersion: goobers.dev/v1alpha1
kind: Instance
repos:
  - provider: bitbucket
    owner: acme
    name: web
`, "provider"},
		{"two token sources in one ref", `
apiVersion: goobers.dev/v1alpha1
kind: Instance
repos:
  - provider: github
    owner: acme
    name: web
    token:
      env: GOOBERS_GITHUB_TOKEN
      file: /etc/goobers/token
`, "maxProperties"},
		{"inline token value", `
apiVersion: goobers.dev/v1alpha1
kind: Instance
repos:
  - provider: github
    owner: acme
    name: web
    token:
      value: ghp_notatoken
`, "additionalProperties"},
		{"misspelled runner capabilities key", `
apiVersion: goobers.dev/v1alpha1
kind: Instance
repos:
  - provider: github
    owner: acme
    name: web
    token:
      env: GOOBERS_GITHUB_TOKEN
runner:
  capability: [dotnet@8]
`, "additionalProperties"},
		{"malformed env passthrough name", `
apiVersion: goobers.dev/v1alpha1
kind: Instance
repos:
  - provider: github
    owner: acme
    name: web
    token:
      env: GOOBERS_GITHUB_TOKEN
runner:
  envPassthrough: ["NUGET_CONFIG_FILE=/etc/nuget.config"]
`, "pattern"},
		{"credential grant claiming both capability and mcp", `
apiVersion: goobers.dev/v1alpha1
kind: Instance
repos:
  - provider: github
    owner: acme
    name: web
    token:
      env: GOOBERS_GITHUB_TOKEN
credentials:
  - capability: agent:model
    mcp: acme-docs
    token:
      env: GOOBERS_TOKEN
`, "oneOf"},
		{"unknown capability", `
apiVersion: goobers.dev/v1alpha1
kind: Instance
repos:
  - provider: github
    owner: acme
    name: web
    token:
      env: GOOBERS_GITHUB_TOKEN
credentials:
  - capability: github:pr:approve
    token:
      env: GOOBERS_TOKEN
`, "enum"},
		{"missing repos", `
apiVersion: goobers.dev/v1alpha1
kind: Instance
`, "repos"},
		{"unsupported schemaVersion", `
apiVersion: goobers.dev/v1alpha1
kind: Instance
schemaVersion: 3
repos: []
`, "enum"},
		{"runner entry without host", `
apiVersion: goobers.dev/v1alpha1
kind: Instance
repos: []
runners:
  - name: ci-linux
`, "host"},
		{"runner name that kubernetes could not carry", `
apiVersion: goobers.dev/v1alpha1
kind: Instance
repos: []
runners:
  - name: CI_Linux
    host: self
`, "pattern"},
		{"runner os outside the enum", `
apiVersion: goobers.dev/v1alpha1
kind: Instance
repos: []
runners:
  - name: mac
    host: self
    provides:
      os: darwin
`, "enum"},
		{"runner quantity that is not a quantity", `
apiVersion: goobers.dev/v1alpha1
kind: Instance
repos: []
runners:
  - name: ci
    host: self
    provides:
      cpu: fast
`, "pattern"},
		{"runner restriction outside the closed list", `
apiVersion: goobers.dev/v1alpha1
kind: Instance
repos: []
runners:
  - name: ci
    host: self
    restrictions: [network:proxy]
`, "enum"},
		{"otlp insecure true against a non-loopback endpoint (#3804 if/then)", `
apiVersion: goobers.dev/v1alpha1
kind: Instance
repos: []
telemetry:
  otlp:
    endpoint: goobers-collector.goobers-system:4317
    insecure: true
`, "pattern"},
		{"otlp tls block with an unknown property", `
apiVersion: goobers.dev/v1alpha1
kind: Instance
repos: []
telemetry:
  otlp:
    endpoint: collector.example.com:4317
    tls:
      trustAll: true
`, "additionalProperties"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateInstanceYAML(t, schema, test.document)
			if err == nil {
				t.Fatal("structurally impossible instance.yaml was accepted")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error %v does not mention %q", err, test.want)
			}
		})
	}
}

// TestInstanceSchemaOTLPInsecureLoopbackShapes is the belt-and-braces half of
// #3804: goobers validate (internal/instance.OTLPConfig.Validate,
// TestLoadConfigRejectsInsecureNonLoopbackOTLP) is the actual enforcement,
// but the published schema now encodes the same insecure -> loopback rule
// via if/then so an editor or `goobers schema instance` catches the mistake
// before a load ever runs. This pins that every shape the Go validator
// accepts for insecure:true is also accepted here, and the shape it always
// rejected is rejected here too.
func TestInstanceSchemaOTLPInsecureLoopbackShapes(t *testing.T) {
	schema := compileInstanceSchema(t)

	accepted := []string{
		"http://127.0.0.1:4317",
		"127.0.0.1:4317",
		"http://localhost:4317",
		"localhost:4317",
		"http://LOCALHOST:4317",
		// Bracketed IPv6 loopback spellings net.IP.IsLoopback() (and so
		// isLoopbackHost) accepts but the earlier ::1-only alternative did
		// not: the schema's if/then is belt-and-braces documentation of the
		// Go rule, not a second enforcement boundary, so it must not be
		// stricter than the validator it is describing.
		"[0:0:0:0:0:0:0:1]:4317",
		"http://[0:0:0:0:0:0:0:1]:4317",
		"[::ffff:127.0.0.1]:4317",
	}
	for _, endpoint := range accepted {
		t.Run("accepts "+endpoint, func(t *testing.T) {
			// Double-quoted: some endpoints (bracketed IPv6) start with "["
			// or contain ":", which YAML would otherwise parse as a flow
			// sequence/mapping indicator rather than plain scalar text.
			document := "apiVersion: goobers.dev/v1alpha1\nkind: Instance\nrepos: []\ntelemetry:\n  otlp:\n    endpoint: \"" + endpoint + "\"\n    insecure: true\n"
			if err := validateInstanceYAML(t, schema, document); err != nil {
				t.Fatalf("loopback insecure endpoint %q was rejected: %v", endpoint, err)
			}
		})
	}

	rejected := []string{
		"http://goobers-collector.goobers-system:4317",
		"goobers-collector.goobers-system:4317",
		"http://collector.example.com:4317",
	}
	for _, endpoint := range rejected {
		t.Run("rejects "+endpoint, func(t *testing.T) {
			// Double-quoted: some endpoints (bracketed IPv6) start with "["
			// or contain ":", which YAML would otherwise parse as a flow
			// sequence/mapping indicator rather than plain scalar text.
			document := "apiVersion: goobers.dev/v1alpha1\nkind: Instance\nrepos: []\ntelemetry:\n  otlp:\n    endpoint: \"" + endpoint + "\"\n    insecure: true\n"
			if err := validateInstanceYAML(t, schema, document); err == nil {
				t.Fatalf("non-loopback insecure endpoint %q was accepted", endpoint)
			}
		})
	}

	// insecure absent (or false) leaves the endpoint unconstrained by this
	// rule — a remote TLS endpoint with no insecure key is the documented
	// default shape and must not trip the loopback pattern.
	t.Run("insecure absent leaves remote endpoint unconstrained", func(t *testing.T) {
		document := `
apiVersion: goobers.dev/v1alpha1
kind: Instance
repos: []
telemetry:
  otlp:
    endpoint: collector.example.com:4317
`
		if err := validateInstanceYAML(t, schema, document); err != nil {
			t.Fatalf("remote TLS endpoint with insecure absent was rejected: %v", err)
		}
	})
}

// The cold-start walkthroughs read instance.yaml guidance out of scattered
// prose because the schema carried none. These are the three traps that cost
// the most time; a description that stops naming the consequence stops being
// the answer, so they are asserted rather than assumed.
func TestInstanceSchemaDescriptionsCarryTheColdStartTraps(t *testing.T) {
	raw, err := FS.ReadFile("instance.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	document := string(raw)
	for _, want := range []string{
		// runner.capabilities: claim what gaggles require or runs never schedule.
		"requiredCapabilities",
		"never schedules a single run",
		// runner.envPassthrough: what the built-in allowlist families already cover.
		"Java/Maven/Gradle toolchain families",
		// runner.defaultStageTimeout: sizing against a real build+test entrypoint.
		"built-in 10 minutes",
		// Azure DevOps is a three-part identity.
		"three-part organization/project/repository identity",
	} {
		if !strings.Contains(document, want) {
			t.Errorf("instance schema no longer documents %q", want)
		}
	}
}

// TestSchemaPatternsAreECMA262Portable guards against a Go-only regex
// construct silently breaking every non-Go consumer of these schemas. JSON
// Schema 2020-12 §6.4 mandates the ECMA-262 regex dialect for "pattern",
// which has no inline-flag group ((?i), (?s), (?i:...), and friends) — that
// syntax compiles only under Go's RE2 (via santhosh-tekuri/jsonschema in
// this test suite), so a pattern using it would pass every test here while
// making ajv, Python's jsonschema/check-jsonschema, and the VS Code YAML
// extension reject the WHOLE schema document — not just the rule that used
// it (#3804 review: instance.schema.json:833 shipped exactly this once).
func TestSchemaPatternsAreECMA262Portable(t *testing.T) {
	// Matches an inline-flag group: (?i), (?is), (?i:...), etc. Deliberately
	// does NOT match the ECMA-262-legal constructs that also start "(?" —
	// non-capturing (?:, lookahead (?=/(?!, lookbehind (?<=/(?<!, and named
	// groups (?<name> — those are all followed by ":", "=", "!", or "<",
	// while an inline-flag group is always followed by a bare regex-flag
	// letter (i, s, m, U, ...).
	inlineFlagGroup := regexp.MustCompile(`\(\?[A-Za-z]`)
	for _, file := range Files() {
		raw, err := FS.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		var doc any
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("decode %s: %v", file, err)
		}
		walkSchemaPatterns(doc, "$", func(path, pattern string) {
			if inlineFlagGroup.MatchString(pattern) {
				t.Errorf("%s: pattern %q at %s uses a Go-only inline-flag group — "+
					"not valid ECMA-262 (the dialect JSON Schema 2020-12 mandates) and "+
					"will not compile in ajv/python jsonschema/etc; spell case-"+
					"insensitivity out as character classes instead, e.g. [Hh][Tt][Tt][Pp]",
					file, pattern, path)
			}
		})
	}
}

// walkSchemaPatterns recursively visits every string value found under a
// "pattern" object key anywhere in a decoded JSON Schema document.
func walkSchemaPatterns(node any, path string, fn func(path, pattern string)) {
	switch v := node.(type) {
	case map[string]any:
		for key, val := range v {
			childPath := path + "." + key
			if key == "pattern" {
				if s, ok := val.(string); ok {
					fn(childPath, s)
				}
			}
			walkSchemaPatterns(val, childPath, fn)
		}
	case []any:
		for i, val := range v {
			walkSchemaPatterns(val, fmt.Sprintf("%s[%d]", path, i), fn)
		}
	}
}
