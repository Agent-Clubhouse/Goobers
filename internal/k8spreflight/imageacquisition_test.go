package k8spreflight

import (
	"context"
	"errors"
	"slices"
	"testing"
)

func TestImagePullPolicyNeverFallsBackSilently(t *testing.T) {
	for _, policy := range []string{"", "always", "never", "invalid"} {
		t.Run(policy, func(t *testing.T) {
			var operations []string
			run := func(_ context.Context, cmd overlayCommand) ([]byte, error) {
				operations = append(operations, cmd.Args[0])
				if cmd.Args[0] == "pull" {
					return nil, errors.New("registry denied")
				}
				return []byte("cached image"), nil
			}
			_, err := acquirePinnedImage(context.Background(), run, "docker", imageRequirements{Image: "fixture", PullPolicy: policy})
			if policy == "never" {
				if err != nil || !slices.Equal(operations, []string{"image"}) {
					t.Fatalf("cached lookup=%v err=%v", operations, err)
				}
			} else if err == nil || slices.Contains(operations, "image") {
				t.Fatalf("silent fallback: %v err=%v", operations, err)
			}
		})
	}
}
