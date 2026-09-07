package k8spreflight

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"math/big"
	"slices"
	"strings"
	"testing"
	"time"
)

func imageFixtureRoot(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestPinnedImageProbesUseImmutableArtifactAndExplicitCAEvidence(t *testing.T) {
	sha, id := strings.Repeat("a", 40), "sha256:"+strings.Repeat("b", 64)
	root := imageFixtureRoot(t)
	for _, tc := range []struct {
		name, fail, version string
		ca                  bool
	}{
		{name: "complete", version: sha, ca: true},
		{name: "CA unchecked", version: sha},
		{name: "wrong binary stamp", version: strings.Repeat("c", 40), ca: true},
		{name: "missing binary", fail: "goobers", version: sha, ca: true},
		{name: "missing sidecar executable", fail: "/usr/local/bin/gocache-trim", version: sha, ca: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			containers := map[string]bool{}
			caChecked := false
			run := func(_ context.Context, request overlayCommand) ([]byte, error) {
				switch request.Args[0] {
				case "pull":
					return nil, nil
				case "image":
					return []byte(`[{"Id":"` + id + `","Os":"linux"}]`), nil
				case "rm":
					name := request.Args[2]
					if !containers[name] {
						t.Fatalf("cleanup targeted a container not created by this probe: %q", name)
					}
					delete(containers, name)
					return nil, nil
				case "run":
					containers[request.Args[2]] = true
					if !slices.Contains(request.Args, id) || !slices.Contains(request.Args, "--read-only") || !slices.Contains(request.Args, "none") {
						t.Fatalf("probe is not isolated or immutable: %q", request.Args)
					}
					if tc.fail != "" && slices.Contains(request.Args, tc.fail) {
						return nil, errors.New("fixture executable unavailable")
					}
					if slices.Contains(request.Args, "--version") {
						return []byte(`{"commit":"` + tc.version + `"}`), nil
					}
					if len(request.Input) > 0 {
						caChecked = bytes.Equal(request.Input, root)
					}
					return nil, nil
				default:
					t.Fatalf("unexpected runtime operation: %q", request.Args)
					return nil, nil
				}
			}
			required := imageRequirements{Image: "registry.invalid/goobers:" + sha, Commit: sha, Tools: []string{"git"}, Executables: []string{"/usr/local/bin/gocache-trim"}}
			if tc.ca {
				required.RootCA = root
			}
			observation, err := probePinnedImage(context.Background(), run, "docker", required)
			wantError := tc.fail != "" || tc.version != sha
			if (err != nil) != wantError {
				t.Fatalf("probe=%+v err=%v, wantError=%v", observation, err, wantError)
			}
			if len(containers) != 0 {
				t.Fatalf("probe leaked containers: %v", containers)
			}
			if !wantError && tc.ca && (!caChecked || observation.Checked != 4 || len(observation.Unchecked) != 0) {
				t.Fatalf("complete claim without artifact evidence: %+v CA=%v", observation, caChecked)
			}
			if !wantError && !tc.ca && len(observation.Unchecked) == 0 {
				t.Fatal("missing CA silently counted as verified")
			}
		})
	}
}

func TestImageProbeCancellationStillRemovesOwnedContainer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var name string
	cleaned := false
	run := func(current context.Context, request overlayCommand) ([]byte, error) {
		if request.Args[0] == "run" {
			name = request.Args[2]
			cancel()
			return nil, context.Canceled
		}
		if current.Err() != nil || !slices.Equal(request.Args, []string{"rm", "--force", name}) {
			t.Fatalf("cleanup cancelled or not scoped to owned container: %v %q", current.Err(), request.Args)
		}
		cleaned = true
		return nil, nil
	}
	image := imageRuntime{run: run, runtime: "docker", id: "sha256:" + strings.Repeat("a", 64), os: "linux"}
	if _, err := image.invoke(ctx, "goobers", nil, nil); !errors.Is(err, context.Canceled) || !cleaned {
		t.Fatalf("cancel cleanup: %v cleaned=%v", err, cleaned)
	}
}

func TestOverlayCommandOutputBoundAndPreCancelledContext(t *testing.T) {
	var buffer probeBuffer
	data := bytes.Repeat([]byte("x"), 5<<20)
	if n, err := buffer.Write(data); err != nil || n != len(data) || buffer.Len() != 4<<20 || !buffer.truncated {
		t.Fatalf("output bound: retained=%d truncated=%v n=%d err=%v", buffer.Len(), buffer.truncated, n, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := executeOverlayCommand(ctx, overlayCommand{Program: "must-not-launch"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancelled command: %v", err)
	}
}
