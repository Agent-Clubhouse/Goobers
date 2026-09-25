package recovery

import "testing"

func TestRecoveryCredentialsRejectRepositoryAndConfigRedirection(t *testing.T) {
	for _, environment := range [][]string{
		{"GIT_DIR=/different.git"},
		{"git_work_tree=/different"},
		{"GIT_OBJECT_DIRECTORY=/different"},
		{"GIT_CONFIG_PARAMETERS=core.worktree=/different"},
		{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.worktree", "GIT_CONFIG_VALUE_0=/different"},
		{"GIT_CONFIG_COUNT=1"},
		{"GIT_ASKPASS=first", "git_askpass=second"},
	} {
		if err := validateCredentialEnvironment(environment, "https://example.invalid/repo"); err == nil {
			t.Fatal("unsafe Git credential environment accepted")
		}
	}
}

func TestRecoveryCredentialsPermitAskpassAndDisabledHelper(t *testing.T) {
	for _, environment := range [][]string{
		nil,
		{"PATH=/tools", "GIT_ASKPASS=/private/askpass", "GOOBERS_GIT_TOKEN=fixture", "GIT_TERMINAL_PROMPT=0"},
		{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=credential.helper", "GIT_CONFIG_VALUE_0="},
	} {
		if err := validateCredentialEnvironment(environment, "https://example.invalid/repo"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRecoveryCredentialHeaderMustMatchFetchedRepository(t *testing.T) {
	remote := "https://example.invalid/team/repo.git"
	environment := []string{"GIT_CONFIG_COUNT=2", "GIT_CONFIG_KEY_0=credential.helper", "GIT_CONFIG_VALUE_0=", "GIT_CONFIG_KEY_1=http." + remote + "/.extraheader", "GIT_CONFIG_VALUE_1=AUTHORIZATION: basic fixture"}
	if err := validateCredentialEnvironment(environment, remote); err != nil {
		t.Fatal(err)
	}
	if err := validateCredentialEnvironment(environment, "https://different.invalid/repo"); err == nil {
		t.Fatal("foreign repository authorization header accepted")
	}
	environment[3] = "GIT_CONFIG_KEY_1=http.extraheader"
	if err := validateCredentialEnvironment(environment, remote); err == nil {
		t.Fatal("unscoped authorization header accepted")
	}
}

// An ADO bearer credential adds a third slot carrying only the MSA passthrough
// header on the same repository scope; nothing else may occupy that slot.
func TestRecoveryCredentialsPermitADOBearerPassThroughSlotOnly(t *testing.T) {
	remote := "https://dev.azure.com/example-org/example-project/_git/repo"
	scoped := "http." + remote + "/.extraheader"
	bearer := func() []string {
		return []string{
			"GIT_CONFIG_COUNT=3",
			"GIT_CONFIG_KEY_0=credential.helper", "GIT_CONFIG_VALUE_0=",
			"GIT_CONFIG_KEY_1=" + scoped, "GIT_CONFIG_VALUE_1=AUTHORIZATION: Bearer fixture",
			"GIT_CONFIG_KEY_2=" + scoped, "GIT_CONFIG_VALUE_2=X-VSS-ForceMsaPassThrough: true",
			"GIT_TERMINAL_PROMPT=0",
		}
	}
	if err := validateCredentialEnvironment(bearer(), remote); err != nil {
		t.Fatalf("ADO bearer credential environment rejected: %v", err)
	}
	for name, mutate := range map[string]func([]string) []string{
		"foreign repository":     func(env []string) []string { return env },
		"unscoped third key":     func(env []string) []string { env[5] = "GIT_CONFIG_KEY_2=http.extraheader"; return env },
		"arbitrary third key":    func(env []string) []string { env[5] = "GIT_CONFIG_KEY_2=core.worktree"; return env },
		"other third value":      func(env []string) []string { env[6] = "GIT_CONFIG_VALUE_2=AUTHORIZATION: Bearer other"; return env },
		"count two, three slots": func(env []string) []string { env[0] = "GIT_CONFIG_COUNT=2"; return env },
		"count three, two slots": func(env []string) []string { return append(env[:5:5], env[7:]...) },
		"fourth slot": func(env []string) []string {
			return append(env, "GIT_CONFIG_KEY_3=core.worktree", "GIT_CONFIG_VALUE_3=/different")
		},
	} {
		target := remote
		if name == "foreign repository" {
			target = "https://different.invalid/repo"
		}
		if err := validateCredentialEnvironment(mutate(bearer()), target); err == nil {
			t.Fatalf("%s: unsafe ADO bearer credential environment accepted", name)
		}
	}
}
