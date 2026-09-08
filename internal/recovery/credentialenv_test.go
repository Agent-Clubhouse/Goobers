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
		if err := validateCredentialEnvironment(environment); err == nil {
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
		if err := validateCredentialEnvironment(environment); err != nil {
			t.Fatal(err)
		}
	}
}
