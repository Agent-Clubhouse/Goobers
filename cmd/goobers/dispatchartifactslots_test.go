package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/artifactset"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/livejournal"
)

func TestRemoteCommandNamedPublication(t *testing.T) {
	for _, mode := range []string{"published", "missing", "blob-failure", "journal-failure"} {
		t.Run(mode, func(t *testing.T) {
			var mu sync.Mutex
			blobs := map[string][]byte{}
			adopted := map[string]bool{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				data, _ := io.ReadAll(r.Body)
				if r.Method == http.MethodPut {
					if mode == "blob-failure" {
						w.WriteHeader(http.StatusForbidden)
						return
					}
					blobs[strings.TrimPrefix(r.URL.Path, dispatcher.BlobPathPrefix)] = data
				} else {
					var req livejournal.EmitRequest
					if err := json.Unmarshal(data, &req); err != nil {
						t.Error(err)
					}
					for _, op := range req.Ops {
						if op.Artifact != nil && op.Artifact.Ref != nil {
							if mode == "journal-failure" {
								w.WriteHeader(http.StatusForbidden)
								return
							}
							adopted[op.Artifact.Ref.Digest] = true
						}
					}
				}
				_, _ = io.WriteString(w, `{}`)
			}))
			t.Cleanup(server.Close)
			configurePreparedPodPublication(t, server.URL)
			t.Chdir(t.TempDir())
			contract := apiv1.ArtifactPublication{Stage: "curate", Visit: 17, Slots: []apiv1.ArtifactSlot{{Name: "report"}}}
			encoded, _ := json.Marshal(contract)
			t.Setenv(dispatcher.EnvArtifactPublication, string(encoded))
			t.Setenv(dispatcher.InputEnvVar("artifactManifestFile"), "manifest.json")
			manifest := `{"schemaVersion":"goobers.dev/stage-artifact-set/v1alpha1","entries":[{"name":"report","path":"report","mediaType":"text/plain"}]}`
			if mode == "missing" {
				manifest = `{"schemaVersion":"goobers.dev/stage-artifact-set/v1alpha1","entries":[]}`
			}
			// Real staged command, including an assertion that runner authority
			// cannot leak into the command's environment.
			command, _ := json.Marshal([]string{"sh", "-c", `test -z "$GOOBERS_ARTIFACT_PUBLICATION" || exit 8; printf '%s' '` + manifest + `' > manifest.json; printf evidence > report; printf diagnostic`})
			t.Setenv(dispatcher.EnvStageCommand, string(command))
			result := runDeclaredStage(t.Context(), io.Discard, io.Discard)
			if mode != "published" {
				want := "artifact_publication_failed"
				if mode == "missing" {
					want = artifactset.MissingSlotCode
				}
				if result.Status != apiv1.ResultFailure || result.Error == nil || result.Error.Code != want {
					t.Fatalf("publication failure escaped: %+v", result)
				}
				return
			}
			if result.Status != apiv1.ResultSuccess || len(result.Artifacts) < 3 {
				t.Fatalf("remote result=%+v", result)
			}
			mu.Lock()
			defer mu.Unlock()
			var index artifactset.Index
			if err := json.Unmarshal(blobs[result.Artifacts[0].Digest], &index); err != nil {
				t.Fatal(err)
			}
			if len(index.Bindings) != 1 || index.Bindings[0].Stage != "curate" || index.Bindings[0].Visit != 17 || index.Bindings[0].Attempt != 2 || index.Bindings[0].Artifact != result.Artifacts[1] {
				t.Fatalf("binding=%+v", index)
			}
			for _, pointer := range result.Artifacts[:2] {
				if !adopted[pointer.Digest] || apiv1.Digest(blobs[pointer.Digest]) != pointer.Digest {
					t.Fatalf("pointer returned before durable publication: %+v", pointer)
				}
			}
		})
	}
}
