package dispatcher

import (
	"errors"
	"slices"
	"testing"

	"github.com/goobers/goobers/internal/executor"
)

// gagglebacklogenv_test.go is #5957's dispatcher-side evidence: a gaggle's
// backlog settings (the Azure DevOps backlog project and doneStates) are
// resolved HERE, where the instance config is readable, and stamped into a
// goobers-CLI stage pod as dispatcher-owned run identity. Before, a pod read
// no instance config, so its backlog-query addressed the code project and a
// stricter doneStates was silently ignored.

const exampleGaggleBacklog = `{"provider":"ado","project":"example-backlog","doneStates":{"categories":["Completed"]}}`

func gaggleBacklogConfig() Config {
	cfg := testConfig()
	cfg.GaggleBacklogs = map[string]string{"alpha": exampleGaggleBacklog}
	return cfg
}

func gaggleBacklogAttempt() Attempt {
	attempt := routedCLIAttempt()
	attempt.Gaggle = "alpha"
	return attempt
}

// The headline: a goobers-CLI stage pod carries its own gaggle's settings.
func TestRenderedCLIStagePodCarriesTheGaggleBacklogSettings(t *testing.T) {
	pod, err := RenderPod(gaggleBacklogConfig(), gaggleBacklogAttempt(), linuxRunner())
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	if got := podEnvMap(pod)[GaggleBacklogEnv]; got != exampleGaggleBacklog {
		t.Fatalf("%s = %q, want the gaggle's settings %q", GaggleBacklogEnv, got, exampleGaggleBacklog)
	}
}

// STAMPED UNCONDITIONALLY for a CLI stage, so a runner image cannot supply
// the name. A gaggle the dispatcher has no settings for gets an EMPTY stamp,
// which the CLI refuses rather than defaulting, and another gaggle's settings
// never leak into the attempt.
func TestCLIStagePodAlwaysCarriesTheGaggleBacklogStamp(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Config, *Attempt)
	}{
		{"no settings configured at all", func(c *Config, _ *Attempt) { c.GaggleBacklogs = nil }},
		{"only another gaggle has settings", func(c *Config, _ *Attempt) { c.GaggleBacklogs = map[string]string{"beta": exampleGaggleBacklog} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, attempt := gaggleBacklogConfig(), gaggleBacklogAttempt()
			tc.mutate(&cfg, &attempt)
			pod, err := RenderPod(cfg, attempt, linuxRunner())
			if err != nil {
				t.Fatalf("RenderPod: %v", err)
			}
			value, stamped := podEnvMap(pod)[GaggleBacklogEnv]
			if !stamped {
				t.Fatalf("%s was not stamped; a runner image could supply it instead", GaggleBacklogEnv)
			}
			if value != "" {
				t.Fatalf("%s = %q, want empty: nothing was resolved for this gaggle", GaggleBacklogEnv, value)
			}
		})
	}
}

// The dispatcher's name and the executor's (which the local executor refuses
// in a workflow's env) must not drift apart.
func TestGaggleBacklogEnvMatchesExecutor(t *testing.T) {
	if GaggleBacklogEnv != executor.GaggleBacklogEnvVar {
		t.Fatalf("GaggleBacklogEnv = %q, executor.GaggleBacklogEnvVar = %q", GaggleBacklogEnv, executor.GaggleBacklogEnvVar)
	}
}

// A non-CLI stage never sees it, and a goobers-CLI stage keeps it.
func TestGaggleBacklogIsRunIdentity(t *testing.T) {
	attempt := gaggleBacklogAttempt()
	attempt.CLIStage = false
	pod, err := RenderPod(gaggleBacklogConfig(), attempt, linuxRunner())
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	if value, stamped := podEnvMap(pod)[GaggleBacklogEnv]; stamped {
		t.Fatalf("%s = %q on a non-CLI stage", GaggleBacklogEnv, value)
	}
	if !slices.Contains(DispatcherControlEnv, GaggleBacklogEnv) {
		t.Fatalf("%s is not in DispatcherControlEnv, so a non-CLI stage would inherit an image-baked value", GaggleBacklogEnv)
	}
	if !slices.Contains(DispatcherRunIdentityEnv, GaggleBacklogEnv) {
		t.Fatalf("%s is not run identity, so a goobers-CLI stage would be stripped of the settings it is stamped to apply", GaggleBacklogEnv)
	}
}

// A workflow cannot author its own gaggle's backlog settings, by name or by
// $(VAR) dereference.
func TestWorkflowEnvCannotAuthorTheGaggleBacklog(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"by name":               {GaggleBacklogEnv: `{"project":"attacker"}`},
		"by $(VAR) dereference": {"MINE": "$(" + GaggleBacklogEnv + ")"},
	} {
		t.Run(name, func(t *testing.T) {
			attempt := gaggleBacklogAttempt()
			attempt.Env = env
			_, err := RenderPod(gaggleBacklogConfig(), attempt, linuxRunner())
			var override *ControlEnvOverrideError
			if !errors.As(err, &override) {
				t.Fatalf("RenderPod error = %v, want a ControlEnvOverrideError", err)
			}
		})
	}
}
