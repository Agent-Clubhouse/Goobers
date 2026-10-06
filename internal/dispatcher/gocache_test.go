package dispatcher

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
)

func mountFor(pod *corev1.Pod, volume string) (string, bool) {
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		if m.Name == volume {
			return m.MountPath, true
		}
	}
	return "", false
}

func volumeNamed(pod *corev1.Pod, name string) *corev1.Volume {
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == name {
			return &pod.Spec.Volumes[i]
		}
	}
	return nil
}

// GOCACHE must never stay on the size-limited tmp:ephemeral tmpfs: the
// operator image bakes GOCACHE=/tmp/gocache and one Go build filled the 512Mi
// tmpfs, starving recovery custody's git directory. It lands on a disk-backed
// emptyDir whether the module cache is the shared claim or the #5595 fallback.
func TestRenderForPlacesGoCacheOnDiskBackedEmptyDir(t *testing.T) {
	for _, tc := range []struct {
		name    string
		runner  RunnerSpec
		path    string
		claimOK bool
	}{
		{"linux-claim", linuxRunner(), LinuxGoBuildCachePath, true},
		{"linux-no-claim", linuxRunner(), LinuxGoBuildCachePath, false},
		{"windows-claim", windowsRunner(), WindowsGoBuildCachePath, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pods := &fakePodAPI{}
			if !tc.claimOK {
				pods.claimErr = apierrors.NewNotFound(claimResource, goBuildCacheClaim)
			}
			d, _, _ := goModCacheDispatcher(t, pods)
			pod, err := d.renderFor(context.Background(), testAttempt(), tc.runner)
			if err != nil {
				t.Fatalf("renderFor: %v", err)
			}
			if got := podEnv(pod)["GOCACHE"]; got != tc.path {
				t.Fatalf("GOCACHE = %q, want %q", got, tc.path)
			}
			if got, ok := mountFor(pod, goCompileCacheVolume); !ok || got != tc.path {
				t.Fatalf("go-compile-cache mount = %q (%v), want %q", got, ok, tc.path)
			}
			v := volumeNamed(pod, goCompileCacheVolume)
			if v == nil || v.EmptyDir == nil || v.EmptyDir.Medium != "" || v.PersistentVolumeClaim != nil {
				t.Fatalf("go-compile-cache volume = %+v, want a disk-backed emptyDir", v)
			}
			if tmp, ok := mountFor(pod, "tmp"); ok && tmp == tc.path {
				t.Fatalf("GOCACHE shares the tmp mount %q", tmp)
			}
		})
	}
}

func TestConfigTmpfsSizeLimitWiresIntoTmpVolume(t *testing.T) {
	cfg := testConfig()
	cfg.TmpfsSizeLimit = resource.MustParse("2Gi")
	pod, err := RenderPod(cfg, testAttempt(), linuxRunner())
	if err != nil {
		t.Fatal(err)
	}
	tmp := volumeNamed(pod, "tmp")
	if tmp == nil || tmp.EmptyDir.SizeLimit.Cmp(resource.MustParse("2Gi")) != 0 {
		t.Fatalf("tmp volume = %+v, want sizeLimit 2Gi", tmp)
	}
}
