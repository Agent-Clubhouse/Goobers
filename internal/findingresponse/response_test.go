package findingresponse

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/learning"
)

func TestValidateRequiresEveryTriggeringFinding(t *testing.T) {
	findings := []apiv1.Finding{
		{Severity: apiv1.SeverityError, Message: "first diagnostic"},
		{Severity: apiv1.SeverityError, Message: "second diagnostic"},
	}
	if _, err := Validate(findings, "1: addressed: fixed the first diagnostic"); err == nil ||
		!strings.Contains(err.Error(), "second diagnostic") {
		t.Fatalf("partial account error = %v, want the omitted diagnostic named", err)
	}
	if _, err := Validate(findings, "1: addressed: fixed the first diagnostic\n2: addressed: fixed the second diagnostic"); err != nil {
		t.Fatalf("complete account rejected: %v", err)
	}
	if _, err := Validate(findings, "1: addressed: fixed it\n2: addressed: fixed it\n3: addressed: invented finding"); err == nil ||
		!strings.Contains(err.Error(), "triggering episode has 2") {
		t.Fatalf("extra finding response error = %v", err)
	}
	if _, err := Validate(findings[:1], "1: declined: no change needed"); err == nil ||
		!strings.Contains(err.Error(), "obsolete, environmental, flaky, or non-actionable") {
		t.Fatalf("unclassified decline error = %v", err)
	}
	if _, err := Validate(findings[:1], "1: declined: obsolete because the file was removed"); err != nil {
		t.Fatalf("classified decline rejected: %v", err)
	}
}

func TestValidateWithAdditionalPreservesRemediationCycleFindings(t *testing.T) {
	responses, err := ValidateWithAdditional(
		[]apiv1.Finding{{Message: "triggering finding"}},
		`[{"finding":1,"disposition":"addressed","detail":"fixed trigger"},`+
			`{"finding":2,"disposition":"addressed","detail":"fixed later reviewer finding"}]`,
	)
	if err != nil {
		t.Fatalf("ValidateWithAdditional() error = %v", err)
	}
	if len(responses) != 2 || responses[0].Finding != 1 || responses[1].Finding != 2 {
		t.Fatalf("responses = %+v, want trigger plus later finding", responses)
	}
}

func TestValidateResultFailsClosedOnUnreadableLearningEpisode(t *testing.T) {
	pointer := apiv1.ContextPointer{
		Name: "learning.episode[42]",
		Artifact: &apiv1.ArtifactPointer{
			Path: "artifacts/episode.json", Digest: "sha256:episode",
		},
	}
	if _, err := ValidateResult([]apiv1.ContextPointer{pointer}, apiv1.ResultEnvelope{}, func(apiv1.ArtifactPointer) ([]byte, error) {
		return nil, errors.New("artifact unavailable")
	}); err == nil || !strings.Contains(err.Error(), "read learning episode") {
		t.Fatalf("ValidateResult unreadable episode error = %v", err)
	}
}

func TestValidateResultAccountsForLatestLearningEpisode(t *testing.T) {
	episode := learning.Episode{
		Schema: learning.EpisodeSchema, ID: "episode-42", SourceSeq: 42,
		Findings: []apiv1.Finding{{Message: "fix the lint failure"}},
	}
	data, err := json.Marshal(episode)
	if err != nil {
		t.Fatal(err)
	}
	pointer := apiv1.ContextPointer{
		Name: "learning.episode[42]",
		Artifact: &apiv1.ArtifactPointer{
			Path: "artifacts/episode.json", Digest: "sha256:episode",
		},
	}
	got, err := ValidateResult([]apiv1.ContextPointer{pointer}, apiv1.ResultEnvelope{
		Outputs: map[string]interface{}{Output: "1: addressed: added the required documentation"},
	}, func(apiv1.ArtifactPointer) ([]byte, error) {
		return data, nil
	})
	if err != nil {
		t.Fatalf("ValidateResult: %v", err)
	}
	if got.ID != episode.ID || len(got.Findings) != 1 {
		t.Fatalf("projected episode = %+v, want %+v", got, episode)
	}
}
