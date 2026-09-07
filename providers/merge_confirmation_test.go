package providers

import "testing"

func TestMergeConfirmationUsesCredentialFreeRepositoryRoute(t *testing.T) {
	confirmation := newMergeConfirmation("https://token:secret@Forge.Example/install-one/repos/acme/app/?api-version=7.1&token=secret#private", "9", "commit")
	if confirmation == nil || confirmation.RepositoryAPIURL != "https://forge.example/install-one/repos/acme/app" {
		t.Fatalf("confirmation = %+v", confirmation)
	}
	other := newMergeConfirmation("https://forge.example/install-two/repos/acme/app", "9", "commit")
	if other.RepositoryAPIURL == confirmation.RepositoryAPIURL {
		t.Fatal("separate installations on one host collided")
	}
	escaped := newMergeConfirmation("https://forge.example/repos/a%2Fb/app", "9", "commit")
	plain := newMergeConfirmation("https://forge.example/repos/a/b/app", "9", "commit")
	if escaped.RepositoryAPIURL == plain.RepositoryAPIURL {
		t.Fatal("escaped repository path segments lost their identity")
	}
	for _, bad := range []string{"", "/relative", "file:///repo", "https://", "%invalid"} {
		if got := newMergeConfirmation(bad, "9", "commit"); got != nil {
			t.Errorf("invalid repository address %q gained confirmation: %+v", bad, got)
		}
	}
}

func TestObservedMergeOperationDoesNotGainConfirmation(t *testing.T) {
	if _, present := MutationRunnerFields("merge", nil, nil)["mergeConfirmation"]; present {
		t.Fatal("legacy operation was upgraded to confirmed landing")
	}
}
