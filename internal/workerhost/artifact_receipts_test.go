package workerhost

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
)

func TestStagingArtifactReceiptsPreserveRecorderNamesAndScrubbedRefs(t *testing.T) {
	var receipts invoke.ArtifactReceipts
	recorder := NewStagingArtifacts(t.TempDir(), fixedScrubber{}, nil)
	recorder.ReportArtifact = receipts.Record
	for _, name := range []string{"result.json", "another-name.json", "result.json"} {
		ref, err := recorder.RecordArtifact(name, []byte("s3cret"))
		if err != nil {
			t.Fatal(err)
		}
		if ref.Digest != journal.Digest([]byte("«redacted»")) {
			t.Fatal("receipt must address scrubbed bytes")
		}
	}
	got := receipts.Snapshot()
	if len(got) != 3 || got[0].Name != "result.json" || got[1].Name != "another-name.json" || got[2].Name != "result.json" {
		t.Fatalf("receipts = %+v", got)
	}
	if got[0].Ref != got[1].Ref || got[1].Ref != got[2].Ref {
		t.Fatal("identical content changed address")
	}
	// These separate protocols must not create ordinary artifact events.
	if _, err := recorder.RecordSpanWithSchema("work", "trace", "fixture", []byte("span")); err != nil {
		t.Fatal(err)
	}
	if _, err := recorder.RecordPreparedArtifact(t.Context(), "prepared", "text/plain", []byte("prepared")); err != nil {
		t.Fatal(err)
	}
	if len(receipts.Snapshot()) != 3 {
		t.Fatal("span/prepared blob reported as ordinary artifact")
	}
	if _, err := recorder.RecordArtifactBoundedWithIntegrity("bounded", []byte("abc"), apiv1.IntegrityTrusted, 0); err == nil {
		t.Fatal("invalid bound accepted")
	}
	if len(receipts.Snapshot()) != 3 {
		t.Fatal("failed recorder write produced receipt")
	}
}
