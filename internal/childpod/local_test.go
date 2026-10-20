package childpod

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
)

func TestLocalExecutorRequiresActualWriterProofAndRetainsUncertainAttempt(t *testing.T) {
	for _, joined := range []bool{false, true} {
		t.Run(map[bool]string{false: "unobservable", true: "joined"}[joined], func(t *testing.T) {
			request := requestFixture()
			blobs, err := blobstore.NewDir(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			started, returned := false, false
			executor := LocalExecutor{Root: t.TempDir(), Blobs: blobs, Recorder: &recordFake{},
				BeforeStart: func(_ context.Context, r Request, c Contract, digest string) error {
					if digest == "" || r.Attempt.ChildExecutionDigest != digest || c.Stage != request.Attempt.Stage {
						t.Fatal("unbound local contract")
					}
					started = true
					return nil
				},
				Returned: func(context.Context, Request, journal.Ref) error { returned = true; return nil },
			}
			ctx, err := credentials.WithChildCeiling(t.Context(), request.Ceiling)
			if err != nil {
				t.Fatal(err)
			}
			ctx, proof := invoke.WithWorkspaceQuiescence(ctx)
			var workspace string
			_, err = executor.Execute(ctx, request, func(ctx context.Context, path string) (dispatcher.SurrenderedResult, error) {
				workspace = path
				if !started || !credentials.TrustedLocalProcess(ctx) {
					t.Fatal("launch without retained custody or delegated environment")
				}
				if _, err := os.Stat(path); err != nil {
					t.Fatal(err)
				}
				if joined {
					invoke.RegisterWorkspaceWriter(ctx)(nil)
				}
				return dispatcher.SurrenderedResult{Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}}, nil
			})
			if joined {
				if err != nil || proof.Verify() != nil || !returned {
					t.Fatalf("joined result refused: %v %v", err, proof.Verify())
				}
				if _, err := os.Stat(workspace); !os.IsNotExist(err) {
					t.Fatal("settled attempt directory retained")
				}
			} else {
				if !errors.Is(err, invoke.ErrChildCustodyPending) || proof.Verify() == nil || returned {
					t.Fatalf("unobservable writer accepted: %v", err)
				}
				if _, err := os.Stat(workspace); err != nil {
					t.Fatal("unacknowledged attempt was deleted")
				}
			}
		})
	}
}

func TestLocalExecutorFreshAttemptAndCancellationStillReturnCustody(t *testing.T) {
	request := requestFixture()
	blobs, err := blobstore.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var returned int
	executor := LocalExecutor{Root: t.TempDir(), Blobs: blobs, Recorder: &recordFake{}, BeforeStart: func(context.Context, Request, Contract, string) error { return nil }, Returned: func(context.Context, Request, journal.Ref) error { returned++; return nil }}
	var previous string
	for attempt := 1; attempt <= 2; attempt++ {
		request.Attempt.Number = attempt
		request.Attempt.PodAttempt++
		ctx, err := credentials.WithChildCeiling(t.Context(), request.Ceiling)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(ctx)
		ctx, proof := invoke.WithWorkspaceQuiescence(ctx)
		_, err = executor.Execute(ctx, request, func(ctx context.Context, path string) (dispatcher.SurrenderedResult, error) {
			if path == previous {
				t.Fatal("retry reused previous checkout")
			}
			previous = path
			if _, err := os.Stat(filepath.Join(path, "previous-attempt")); !os.IsNotExist(err) {
				t.Fatal("retry inherited attempt files")
			}
			if err := os.WriteFile(filepath.Join(path, "previous-attempt"), []byte("dirty"), 0600); err != nil {
				t.Fatal(err)
			}
			invoke.RegisterWorkspaceWriter(ctx)(nil)
			cancel()
			return dispatcher.SurrenderedResult{}, context.Canceled
		})
		cancel()
		if !errors.Is(err, context.Canceled) || proof.Verify() != nil {
			t.Fatalf("cancelled return lost custody: %v %v", err, proof.Verify())
		}
	}
	if returned != 2 {
		t.Fatalf("return count=%d", returned)
	}
}
