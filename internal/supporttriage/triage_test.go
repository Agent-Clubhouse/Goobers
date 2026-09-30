package supporttriage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/diagnostics"
)

func TestClassifyCoversDistinctDispositions(t *testing.T) {
	cases := []struct {
		name   string
		bundle diagnostics.Bundle
		want   Disposition
	}{
		{"workflow configuration", diagnostics.Bundle{Instance: diagnostics.InstanceInfo{ConfigIssues: []string{"ERROR DVL001 workflow missing required stage"}}}, DispositionWorkflowConfiguration},
		{"harness authentication", bundleWithError("run", "github_forbidden", "credential unauthorized"), DispositionHarnessAuthentication},
		{"local environment", bundleWithError("run", "exec_failed", "echo not on PATH"), DispositionLocalEnvironment},
		{"external dependency", bundleWithError("run", "provider_rate_limit", "provider rate limit"), DispositionExternalDependency},
		{"transient failure", bundleWithError("run", "timeout", "temporary timeout, try again"), DispositionTransientFailure},
		{"unsupported version", diagnostics.Bundle{Binary: diagnostics.BinaryInfo{Version: "v0.1.0"}}, DispositionUnsupportedVersion},
		{"duplicate or known", bundleWithDecision("run", "duplicate of known issue #123"), DispositionDuplicateOrKnown},
		{"not reproduced", bundleWithDecision("run", "not reproduced on clean environment"), DispositionNotReproduced},
		{"insufficient", diagnostics.Bundle{}, DispositionInsufficientEvidence},
		{"goobers defect", supportedBundle(bundleWithError("run", "internal", "goobers invariant violation reproduced on supported version")), DispositionGoobersDefectCandidate},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.bundle)
			if got.Schema != Schema {
				t.Fatalf("schema = %q, want %q", got.Schema, Schema)
			}
			if got.Disposition != tc.want {
				t.Fatalf("disposition = %q, want %q; result=%+v", got.Disposition, tc.want, got)
			}
		})
	}
}

func TestClassifyDoesNotTreatUnrelatedMissingCredentialAsAuth(t *testing.T) {
	missing := false
	bundle := diagnostics.Bundle{Credentials: []diagnostics.CredentialPresence{{
		Capability: "github:pr:write",
		Present:    &missing,
	}}}
	got := Classify(bundle)
	if got.Disposition != DispositionInsufficientEvidence {
		t.Fatalf("disposition = %q, want insufficient_evidence; result=%+v", got.Disposition, got)
	}
}

func TestClassifyFailsClosedForContradictoryEvidence(t *testing.T) {
	bundle := bundleWithError("run", "goobers_internal", "goobers invariant violation reproduced on supported version")
	bundle.Runs[0].Decisions = append(bundle.Runs[0].Decisions, diagnostics.Decision{
		Stage: "support", Subject: "run", Reason: "insufficient evidence: reproduction is ambiguous",
	})
	got := Classify(bundle)
	if got.Disposition != DispositionInsufficientEvidence {
		t.Fatalf("disposition = %q, want insufficient_evidence; result=%+v", got.Disposition, got)
	}
}

func TestClassifyDoesNotPromoteGenericPanic(t *testing.T) {
	bundle := bundleWithError("run", "panic", "provider subprocess panic: should never happen")
	got := Classify(bundle)
	if got.Disposition != DispositionInsufficientEvidence {
		t.Fatalf("disposition = %q, want insufficient_evidence; result=%+v", got.Disposition, got)
	}
}

func TestClassifyDoesNotPromoteBareInternalMarker(t *testing.T) {
	bundle := bundleWithError("run", "internal", "goobers internal error")
	got := Classify(bundle)
	if got.Disposition != DispositionInsufficientEvidence {
		t.Fatalf("disposition = %q, want insufficient_evidence; result=%+v", got.Disposition, got)
	}
}

func TestClassifyDoesNotTreatNotesAsRootCause(t *testing.T) {
	bundle := diagnostics.Bundle{Notes: []string{"could not read daemon state: permission denied"}}
	got := Classify(bundle)
	if got.Disposition != DispositionInsufficientEvidence {
		t.Fatalf("disposition = %q, want insufficient_evidence; result=%+v", got.Disposition, got)
	}
}

func TestClassifyUnsupportedVersionUsesBinaryVersion(t *testing.T) {
	bundle := diagnostics.Bundle{
		Binary: diagnostics.BinaryInfo{Version: "v0.2.0"},
		Runs:   []diagnostics.RunInfo{{RunID: "run", Workflow: "wf", Gaggle: "gaggle"}},
	}
	got := Classify(bundle)
	if got.Disposition != DispositionUnsupportedVersion {
		t.Fatalf("disposition = %q, want unsupported_version; result=%+v", got.Disposition, got)
	}
}

func TestVersionSupportWindowIncludesCurrentPrereleaseAndPreviousStable(t *testing.T) {
	for _, version := range []string{"v0.5.0", "v0.5.0-beta.11", "v0.4.5", "v0.4.0-rc.1"} {
		if versionUnsupported(version) {
			t.Fatalf("versionUnsupported(%q) = true, want false", version)
		}
	}
	for _, version := range []string{"v0.3.3", "v0.2.0", "v0.1.0-beta.2"} {
		if !versionUnsupported(version) {
			t.Fatalf("versionUnsupported(%q) = false, want true", version)
		}
	}
}

func TestClassifyDoesNotPromoteProductCandidateWithoutSupportedBinaryVersion(t *testing.T) {
	bundle := bundleWithError("run", "goobers_internal", "goobers invariant violation reproduced on supported version")
	got := Classify(bundle)
	if got.Disposition != DispositionInsufficientEvidence {
		t.Fatalf("disposition = %q, want insufficient_evidence; result=%+v", got.Disposition, got)
	}
}

func TestClassifyDoesNotPromoteProductCandidateWithCollectionGaps(t *testing.T) {
	bundle := bundleWithError("run", "goobers_internal", "goobers invariant violation reproduced on supported version")
	bundle.Binary.Version = "v0.5.0-beta.11"
	bundle.Notes = []string{"could not collect run artifacts"}
	got := Classify(bundle)
	if got.Disposition != DispositionInsufficientEvidence {
		t.Fatalf("disposition = %q, want insufficient_evidence; result=%+v", got.Disposition, got)
	}
}

func TestClusterFingerprintIsStableAcrossVersionAndPlatform(t *testing.T) {
	left := bundleWithError("run", "goobers_internal", "goobers invariant violation reproduced on supported version")
	left.Binary = diagnostics.BinaryInfo{Version: "v0.5.0-beta.11", OS: "linux", Arch: "amd64"}
	right := bundleWithError("run", "goobers_internal", "goobers invariant violation reproduced on supported version")
	right.Binary = diagnostics.BinaryInfo{Version: "v0.4.5", OS: "windows", Arch: "arm64"}

	leftResult := Classify(left)
	rightResult := Classify(right)
	if leftResult.Cluster.Fingerprint == "" || leftResult.Cluster.Fingerprint != rightResult.Cluster.Fingerprint {
		t.Fatalf("cluster fingerprints = %q and %q, want equal non-empty", leftResult.Cluster.Fingerprint, rightResult.Cluster.Fingerprint)
	}
}

func TestClassifyGoldenOutputs(t *testing.T) {
	cases := []struct {
		name   string
		bundle diagnostics.Bundle
	}{
		{
			name:   "workflow-configuration",
			bundle: diagnostics.Bundle{Instance: diagnostics.InstanceInfo{ConfigIssues: []string{"ERROR DVL001 workflow missing required stage"}}},
		},
		{
			name:   "harness-authentication",
			bundle: bundleWithError("run-auth", "github_forbidden", "credential unauthorized"),
		},
		{
			name:   "local-environment",
			bundle: bundleWithError("run-local", "exec_failed", "echo not on PATH"),
		},
		{
			name:   "external-dependency",
			bundle: bundleWithError("run-provider", "provider_rate_limit", "provider rate limit"),
		},
		{
			name:   "transient-failure",
			bundle: bundleWithError("run-transient", "timeout", "temporary timeout, try again"),
		},
		{
			name:   "unsupported-version",
			bundle: diagnostics.Bundle{Binary: diagnostics.BinaryInfo{Version: "v0.2.0"}, Runs: []diagnostics.RunInfo{{RunID: "run-version", Workflow: "workflow", Gaggle: "gaggle"}}},
		},
		{
			name:   "duplicate-or-known",
			bundle: bundleWithDecision("run-known", "duplicate of known issue #123"),
		},
		{
			name:   "not-reproduced",
			bundle: bundleWithDecision("run-not-reproduced", "not reproduced on clean environment"),
		},
		{
			name:   "insufficient-evidence",
			bundle: diagnostics.Bundle{},
		},
		{
			name: "goobers-defect-candidate",
			bundle: func() diagnostics.Bundle {
				bundle := bundleWithError("run-goobers", "goobers_internal", "goobers invariant violation reproduced on supported version")
				bundle.Binary.Version = "v0.5.0-beta.11"
				return bundle
			}(),
		},
		{
			name: "contradictory-fail-closed",
			bundle: func() diagnostics.Bundle {
				bundle := bundleWithError("run-ambiguous", "goobers_internal", "goobers invariant violation reproduced on supported version")
				bundle.Binary.Version = "v0.5.0-beta.11"
				bundle.Runs[0].Decisions = []diagnostics.Decision{{
					Stage: "support", Subject: "run-ambiguous", Reason: "insufficient evidence: reproduction is ambiguous",
				}}
				return bundle
			}(),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.MarshalIndent(Classify(tc.bundle), "", "  ")
			if err != nil {
				t.Fatalf("marshal result: %v", err)
			}
			got = append(got, '\n')
			path := filepath.Join("testdata", tc.name+".golden.json")
			if os.Getenv("UPDATE_GOLDEN") == "1" {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			wantText := strings.ReplaceAll(string(want), "\r\n", "\n")
			gotText := string(got)
			if gotText != wantText {
				t.Fatalf("support triage golden differs from %s\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
			}
		})
	}
}

func bundleWithError(runID, code, message string) diagnostics.Bundle {
	return diagnostics.Bundle{Runs: []diagnostics.RunInfo{{
		RunID: runID, Workflow: "workflow", Gaggle: "gaggle", Phase: "failed",
		DecisiveError: &diagnostics.ErrorInfo{Stage: "stage", Code: code, Message: message},
	}}}
}

func bundleWithDecision(runID, reason string) diagnostics.Bundle {
	return diagnostics.Bundle{Runs: []diagnostics.RunInfo{{
		RunID: runID, Workflow: "workflow", Gaggle: "gaggle",
		Decisions: []diagnostics.Decision{{Stage: "support", Subject: runID, Reason: reason}},
	}}}
}

func supportedBundle(bundle diagnostics.Bundle) diagnostics.Bundle {
	bundle.Binary.Version = "v0.5.0-beta.11"
	return bundle
}
