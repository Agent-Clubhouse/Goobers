package harness

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/artifactset"
	"github.com/goobers/goobers/internal/journal"
)

type preparedRecorderProbe struct {
	fakeRecorder
	records  [][]byte
	media    []string
	failAt   int
	ordinary int
}

var errPreparedPublication = errors.New("durable publication failed")

func (r *preparedRecorderProbe) RecordArtifact(string, []byte) (journal.Ref, error) {
	r.ordinary++
	return journal.Ref{}, errors.New("prepared set used diagnostic recorder")
}

func (r *preparedRecorderProbe) RecordPreparedArtifact(ctx context.Context, _ string, media string, data []byte) (journal.Ref, error) {
	if err := ctx.Err(); err != nil {
		return journal.Ref{}, err
	}
	r.records = append(r.records, append([]byte(nil), data...))
	r.media = append(r.media, media)
	if len(r.records) == r.failAt {
		return journal.Ref{}, errPreparedPublication
	}
	return journal.ArtifactRef(data)
}

func TestManifestUsesDurableRecorderWithoutPartialResults(t *testing.T) {
	for _, review := range []bool{false, true} {
		for _, failAt := range []int{0, 2, 3} {
			rec := &preparedRecorderProbe{failAt: failAt}
			adapter := &FakeAdapter{Act: func(_ context.Context, req RunRequest) error {
				manifest := artifactset.Manifest{SchemaVersion: artifactset.SchemaVersion, Entries: []artifactset.ManifestEntry{
					{Name: "a.empty", Path: "empty", MediaType: "text/plain"},
					{Name: "artifact-set.json", Path: "payload", MediaType: "text/plain"},
				}}
				body, err := json.Marshal(manifest)
				if err != nil {
					return err
				}
				for name, data := range map[string][]byte{"manifest.json": body, "empty": {}, "payload": []byte("secret-token-value evidence")} {
					if err := os.WriteFile(filepath.Join(req.Workspace, name), data, 0o600); err != nil {
						return err
					}
				}
				if review {
					return WriteCompletion(req.Workspace, req.CompletionPath, apiv1.Verdict{Decision: apiv1.VerdictPass})
				}
				return WriteCompletion(req.Workspace, req.CompletionPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
			}}
			scrubber := journal.NewRegistryScrubber()
			scrubber.Register([]byte("secret-token-value"))
			e, err := NewExecutor(adapter, testInjector(t, "", "", noopRegistrar{}), rec, rec, rec, scrubber, "")
			if err != nil {
				t.Fatal(err)
			}
			env := testEnvelope(t.TempDir())
			env.Inputs = map[string]interface{}{InputArtifactManifestFile: "manifest.json"}
			var pointers []apiv1.ArtifactPointer
			if review {
				var verdict apiv1.Verdict
				verdict, err = e.Review(context.Background(), env)
				pointers = verdict.Evidence
			} else {
				var result apiv1.ResultEnvelope
				result, err = e.Invoke(context.Background(), env)
				pointers = result.Artifacts
			}
			if rec.ordinary != 0 {
				t.Fatal("prepared set used best-effort recorder")
			}
			if failAt > 0 {
				if !errors.Is(err, errPreparedPublication) || len(pointers) != 0 || len(rec.records) != failAt {
					t.Fatalf("partial set escaped: pointers=%v err=%v records=%d", pointers, err, len(rec.records))
				}
				continue
			}
			if err != nil || len(pointers) != 3 || len(rec.records) != 3 {
				t.Fatalf("publication: pointers=%v err=%v records=%d", pointers, err, len(rec.records))
			}
			if len(rec.records[0]) != 0 || string(rec.records[1]) != "[REDACTED] evidence" || rec.media[2] != "application/json" {
				t.Fatalf("prepared bytes/media changed: %q %v", rec.records, rec.media)
			}
			if pointers[0].Digest != apiv1.Digest(rec.records[2]) {
				t.Fatal("normalized index is not slot zero")
			}
		}
	}
}
