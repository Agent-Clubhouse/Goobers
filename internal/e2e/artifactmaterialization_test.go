package e2e

import (
	"testing"

	"github.com/goobers/goobers/internal/readservice"
)

func producerWithArtifact(stage, digest string) readservice.AttemptList {
	return readservice.AttemptList{
		Stage: stage,
		Attempts: []readservice.StageAttempt{
			{
				Number: 1, Class: "initial",
				Artifacts: []readservice.ArtifactMetadata{{Digest: digest, Stage: stage, Attempt: 1}},
			},
		},
	}
}

func TestAssertArtifactMaterializationPass(t *testing.T) {
	producer := producerWithArtifact("implement", "sha256:abc")
	consumption := ArtifactConsumption{ConsumerStage: "local-ci", MaterializedDigest: "sha256:abc", MaterializedBeforeStart: true}
	got := AssertArtifactMaterialization(producer, "sha256:abc", consumption)
	if got.Verdict != VerdictPass {
		t.Fatalf("Verdict = %v, want pass; detail=%q", got.Verdict, got.Detail)
	}
}

func TestAssertArtifactMaterializationSoftFailClassified(t *testing.T) {
	producer := producerWithArtifact("implement", "sha256:abc")
	consumption := ArtifactConsumption{ConsumerStage: "local-ci", IntegrityCheckFailed: true}
	got := AssertArtifactMaterialization(producer, "sha256:abc", consumption)
	if got.Verdict != VerdictFail {
		t.Fatalf("Verdict = %v, want fail (missing blob, but soft-classified)", got.Verdict)
	}
}

func TestAssertArtifactMaterializationFailsOnDigestMismatch(t *testing.T) {
	producer := producerWithArtifact("implement", "sha256:abc")
	consumption := ArtifactConsumption{ConsumerStage: "local-ci", MaterializedDigest: "sha256:zzz", MaterializedBeforeStart: true}
	got := AssertArtifactMaterialization(producer, "sha256:abc", consumption)
	if got.Verdict != VerdictFail {
		t.Fatalf("Verdict = %v, want fail (digest mismatch)", got.Verdict)
	}
}

func TestAssertArtifactMaterializationFailsWhenNotBeforeStart(t *testing.T) {
	producer := producerWithArtifact("implement", "sha256:abc")
	consumption := ArtifactConsumption{ConsumerStage: "local-ci", MaterializedDigest: "sha256:abc", MaterializedBeforeStart: false}
	got := AssertArtifactMaterialization(producer, "sha256:abc", consumption)
	if got.Verdict != VerdictFail {
		t.Fatalf("Verdict = %v, want fail (materialize-before-stage violated)", got.Verdict)
	}
}

func TestAssertArtifactMaterializationInvalidWhenNeverRecorded(t *testing.T) {
	producer := producerWithArtifact("implement", "sha256:other")
	consumption := ArtifactConsumption{ConsumerStage: "local-ci", MaterializedDigest: "sha256:abc", MaterializedBeforeStart: true}
	got := AssertArtifactMaterialization(producer, "sha256:abc", consumption)
	if got.Verdict != VerdictInvalid {
		t.Fatalf("Verdict = %v, want invalid (digest was never recorded by the producer)", got.Verdict)
	}
}
