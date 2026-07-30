package main

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// A run that produced a v1 or v2 gather artifact before this binary deployed
// must still resume. Rejecting older briefs outright stranded in-flight runs and
// contradicted the retained v1/v2 schemas.
func TestOlderRemediationBriefVersionsAreReadable(t *testing.T) {
	for _, schema := range []string{
		"goobers.dev/remediation-brief/v1",
		"goobers.dev/remediation-brief/v2",
		apiv1.RemediationBriefVersion,
	} {
		if !apiv1.SupportedRemediationBriefVersion(schema) {
			t.Errorf("schema %q is not readable, want accepted for resume", schema)
		}
	}
	if apiv1.SupportedRemediationBriefVersion("goobers.dev/remediation-brief/v99") {
		t.Error("an unknown future schema was accepted, want rejected")
	}
}

// Provenance introduced by v3 is absent in older briefs. It must default to the
// weakest grade, never to trusted — an unlabeled input is the case that has to
// fail closed at admission.
func TestMigratedBriefDefaultsProvenanceToUnapproved(t *testing.T) {
	migrated := apiv1.MigrateRemediationBrief(
		apiv1.RemediationBrief{SelectedNumber: "42"}, "goobers.dev/remediation-brief/v2")
	if migrated.Integrity != apiv1.IntegrityUnapproved {
		t.Errorf("integrity = %q, want %q for a pre-provenance brief", migrated.Integrity, apiv1.IntegrityUnapproved)
	}
	if migrated.Schema != apiv1.RemediationBriefVersion {
		t.Errorf("schema = %q, want it upgraded to %q", migrated.Schema, apiv1.RemediationBriefVersion)
	}
	if migrated.Integrity.Meets(apiv1.IntegrityMaintainer) {
		t.Error("a migrated pre-provenance brief must not satisfy a maintainer minimum")
	}
}

// A current brief is passed through untouched — migration must not overwrite a
// grade the producer actually recorded.
func TestCurrentBriefIsNotRewritten(t *testing.T) {
	brief := apiv1.RemediationBrief{
		Schema:         apiv1.RemediationBriefVersion,
		SelectedNumber: "42",
		Integrity:      apiv1.IntegrityMaintainer,
	}
	if got := apiv1.MigrateRemediationBrief(brief, apiv1.RemediationBriefVersion); got.Integrity != apiv1.IntegrityMaintainer {
		t.Errorf("integrity = %q, want the recorded %q preserved", got.Integrity, apiv1.IntegrityMaintainer)
	}
}
