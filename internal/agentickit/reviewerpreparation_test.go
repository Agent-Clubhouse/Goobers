package agentickit

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
)

func TestPreparedReviewerKitBuildSkewAndLegacyDecode(t *testing.T) {
	// N-1 envelopes omit the optional field. N must keep their legacy path.
	legacy, digest, err := Marshal(sampleKit())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(legacy, []byte("preparedReviewerDiff")) {
		t.Fatal("legacy kit acquired new field")
	}
	old, err := Unmarshal(legacy, digest)
	if err != nil || old.Envelope.PreparedReviewerDiff != nil {
		t.Fatalf("legacy decode: %v %v", old, err)
	}
	kit := sampleKit()
	kit.Mode = ModeReview
	kit.Envelope.PreparedReviewerDiff = &apiv1.PreparedReviewerDiff{Observed: true}
	data, digest, err := Marshal(kit)
	if err != nil {
		t.Fatal(err)
	}
	current, err := Unmarshal(data, digest)
	if err != nil || current.Envelope.PreparedReviewerDiff == nil || !current.Envelope.PreparedReviewerDiff.Observed {
		t.Fatalf("current decode: %v %v", current, err)
	}
	// The worker produces this kit and __dispatch-exec consumes it. A rainbow
	// rollout cannot pair an N worker with an N-1 image: dispatch verifies stamps.
	for _, channel := range []struct{ commit, version, image string }{
		{strings.Repeat("a", 40), "dev", "image:" + strings.Repeat("b", 40)},
		{strings.Repeat("a", 40), "v2.0.0", "image:v1.0.0"},
	} {
		if err := dispatcher.VerifySkew(channel.commit, channel.version, channel.image); err == nil {
			t.Fatal("N worker admitted N-1 consumer")
		}
	}
	if err := dispatcher.VerifySkew(strings.Repeat("a", 40), "dev", "image:"+strings.Repeat("a", 40)); err != nil {
		t.Fatal(err)
	}
	// Removing the field from an N kit is the N-1 wire shape, not a new default.
	var wire map[string]any
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	delete(wire["envelope"].(map[string]any), "preparedReviewerDiff")
	data, err = json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	old, err = Unmarshal(data, Digest(data))
	if err != nil || old.Envelope.PreparedReviewerDiff != nil {
		t.Fatalf("absent field: %v %v", old, err)
	}
}
