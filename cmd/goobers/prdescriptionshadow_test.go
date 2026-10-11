package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/decisiongate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/testgit"
)

func TestPRDescriptionStateUsesDeterministicDiffFacts(t *testing.T) {
	repo := t.TempDir()
	runPRDescriptionGit(t, repo, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "client.go"), []byte("package client\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runPRDescriptionGit(t, repo, "add", ".")
	runPRDescriptionGit(t, repo, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "commit", "-m", "seed")
	runPRDescriptionGit(t, repo, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(repo, "client.go"), []byte("package client\n\nfunc retry() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "retry_test.go"), []byte("package client\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runPRDescriptionGit(t, repo, "add", ".")
	runPRDescriptionGit(t, repo, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "commit", "-m", "add retry")

	t.Chdir(repo)
	state, err := prDescriptionState("main", "Add retry", "Retries requests.")
	if err != nil {
		t.Fatal(err)
	}
	if state.SizeBucket != "1-50" || state.AddedLines != 3 || state.DeletedLines != 0 ||
		len(state.ChangedFiles) != 2 || state.Patch == "" || state.PatchTruncated {
		t.Fatalf("state = %+v", state)
	}
}

func TestPRDiffSizeBucket(t *testing.T) {
	for _, tc := range []struct {
		files, lines int
		want         string
	}{
		{0, 0, "empty"},
		{1, 0, "1-50"},
		{2, 50, "1-50"},
		{1, 51, "51-500"},
		{1, 500, "51-500"},
		{1, 501, "501+"},
	} {
		if got := prDiffSizeBucket(tc.files, tc.lines); got != tc.want {
			t.Errorf("prDiffSizeBucket(%d, %d) = %q, want %q", tc.files, tc.lines, got, tc.want)
		}
	}
}

func TestPRDescriptionDefaultThresholdIsAvailable(t *testing.T) {
	if decisiongate.DefaultPRDescriptionAgreementThreshold.Accept <=
		decisiongate.DefaultPRDescriptionAgreementThreshold.Reject {
		t.Fatal("description agreement threshold does not preserve an unsure range")
	}
}

func TestObservePRDescriptionShadowIsOptInAndAdvisory(t *testing.T) {
	root := initDemo(t)
	var stderr bytes.Buffer
	called := false
	inRepo := func(fn func() error) error {
		called = true
		return fn()
	}
	body := observePRDescriptionShadow(root, "run-1", "feature", "main", "Title", "Body", inRepo, &stderr)
	if called || stderr.Len() != 0 || body != "Body" {
		t.Fatalf("off-by-default observer changed publication: called=%v body=%q stderr=%q", called, body, stderr.String())
	}

	cfg, err := instance.LoadConfig(layoutFor(root).ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	cfg.DecisionGate = &decisiongate.Settings{
		Mode: decisiongate.ModeShadow, BaseURLEnv: "PR_SHADOW_URL",
		KeyEnv: "PR_SHADOW_KEY", ModelEnv: "PR_SHADOW_MODEL",
		Fallback: decisiongate.FallbackAgent,
	}
	if err := instance.WriteConfig(layoutFor(root).ConfigFile(), cfg); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PR_SHADOW_URL", "")
	t.Setenv("PR_SHADOW_KEY", "")
	t.Setenv("PR_SHADOW_MODEL", "")
	body = observePRDescriptionShadow(root, "run-1", "feature", "main", "Title", "Body", inRepo, &stderr)
	if called || body != "Body" || !bytes.Contains(stderr.Bytes(), []byte("decisionGate PR description shadow disabled")) {
		t.Fatalf("unavailable scorer changed the path: called=%v body=%q stderr=%q", called, body, stderr.String())
	}
}

func TestAppendPRDescriptionReviewNote(t *testing.T) {
	got := appendPRDescriptionReviewNote(
		"Implements retries.\n",
		decisiongate.PRDescriptionState{SizeBucket: "51-500"},
		decisiongate.Outcome{Decision: decisiongate.Yes, Probability: 0.937},
	)
	want := "Implements retries.\n\n> **Goobers decision gate (shadow):** description/diff agreement `yes` (score 0.937; diff size `51-500`). Advisory only."
	if got != want {
		t.Fatalf("review note = %q, want %q", got, want)
	}
}

func runPRDescriptionGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := testgit.Command(append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}
