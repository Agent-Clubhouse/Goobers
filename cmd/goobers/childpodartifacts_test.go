package main

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/journal"
)

func TestContainedOutputAdoptionCapsProvenanceAndPreservesByteClaims(t *testing.T) {
	for _, grade := range []apiv1.Integrity{"", apiv1.IntegrityTrusted, apiv1.IntegrityMaintainer, apiv1.IntegrityDerived, apiv1.IntegrityUnapproved} {
		t.Run(string(grade), func(t *testing.T) {
			store, err := blobstore.NewDir(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			data := []byte("pod-authored output")
			digest := journal.Digest(data)
			if err := store.Put(t.Context(), digest, data); err != nil {
				t.Fatal(err)
			}
			run, err := journal.Create(t.TempDir(), journal.RunIdentity{RunID: "output-test"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = run.Close() }()
			pointer := apiv1.ArtifactPointer{Path: "outputs/evidence", Digest: digest, Size: int64(len(data)), Integrity: grade}
			out := dispatcher.SurrenderedResult{Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Integrity: grade, Artifacts: []apiv1.ArtifactPointer{pointer}, Transcript: &pointer}, Verdict: &apiv1.Verdict{Evidence: []apiv1.ArtifactPointer{pointer}}}
			if err := adoptContainedPodOutputs(t.Context(), run, store, &out); err != nil {
				t.Fatal(err)
			}
			want := apiv1.IntegrityDerived
			if grade == apiv1.IntegrityUnapproved {
				want = grade
			}
			if out.Result.Integrity != want || out.Result.Artifacts[0].Integrity != want || out.Result.Transcript.Integrity != want || out.Verdict.Evidence[0].Integrity != want {
				t.Fatal("pod promoted provenance", out)
			}
			rd, err := journal.OpenReadOnly(run.Dir())
			if err != nil {
				t.Fatal(err)
			}
			events, err := rd.Events()
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range events {
				if event.Type == journal.EventArtifactRecorded && event.Integrity != want {
					t.Fatal("stored provenance changed", event)
				}
			}
			before := run.Seq()
			pointer.Size++
			if err := adoptContainedPodArtifact(t.Context(), run, store, &pointer); err == nil {
				t.Fatal("incorrect declared size accepted")
			}
			if run.Seq() != before {
				t.Fatal("size mismatch wrote journal custody")
			}
		})
	}
}
