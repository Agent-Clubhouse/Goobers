package dispatcher

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// #6758: tmp:ephemeral /tmp is a memory tmpfs by default, and an operator can
// make it a disk-backed emptyDir so Go's $WORK and t.TempDir() fit a real
// build without the size being charged to the pod's memory limit.
func TestRenderPodTmpVolumeMedium(t *testing.T) {
	windows := windowsRunner()
	windows.Restrictions = []string{"tmp:ephemeral"}
	for _, tc := range []struct {
		name       string
		runner     RunnerSpec
		diskBacked bool
		size       string
		wantMedium corev1.StorageMedium
		wantSize   string
		wantMemory string
		wantEnv    map[string]string
	}{
		{"linux-memory-default", linuxRunner(), false, "", corev1.StorageMediumMemory, "512Mi", "4608Mi", map[string]string{"TMPDIR": LinuxTmpPath}},
		{"linux-disk-default", linuxRunner(), true, "", corev1.StorageMediumDefault, "4Gi", "4Gi", map[string]string{"TMPDIR": LinuxTmpPath}},
		{"linux-disk-sized", linuxRunner(), true, "16Gi", corev1.StorageMediumDefault, "16Gi", "4Gi", map[string]string{"TMPDIR": LinuxTmpPath}},
		{"windows-disk-default", windows, true, "", corev1.StorageMediumDefault, "4Gi", "8Gi", map[string]string{"TMP": WindowsTmpPath, "TEMP": WindowsTmpPath}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.TmpDiskBacked = tc.diskBacked
			if tc.size != "" {
				cfg.TmpfsSizeLimit = resource.MustParse(tc.size)
			}
			pod, err := RenderPod(cfg, testAttempt(), tc.runner)
			if err != nil {
				t.Fatalf("RenderPod: %v", err)
			}
			tmp := volumeNamed(pod, "tmp")
			if tmp == nil || tmp.EmptyDir == nil {
				t.Fatalf("tmp volume = %+v, want an emptyDir", tmp)
			}
			if tmp.EmptyDir.Medium != tc.wantMedium {
				t.Fatalf("tmp medium = %q, want %q", tmp.EmptyDir.Medium, tc.wantMedium)
			}
			if tmp.EmptyDir.SizeLimit == nil || tmp.EmptyDir.SizeLimit.Cmp(resource.MustParse(tc.wantSize)) != 0 {
				t.Fatalf("tmp sizeLimit = %v, want %s", tmp.EmptyDir.SizeLimit, tc.wantSize)
			}
			wantPath := LinuxTmpPath
			if tc.runner.OS == "windows" {
				wantPath = WindowsTmpPath
			}
			if got, ok := mountFor(pod, "tmp"); !ok || got != wantPath {
				t.Fatalf("tmp mount = %q (%v), want %q", got, ok, wantPath)
			}
			env := podEnv(pod)
			for name, want := range tc.wantEnv {
				if env[name] != want {
					t.Fatalf("%s = %q, want %q", name, env[name], want)
				}
			}
			memory := pod.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory]
			if memory.Cmp(resource.MustParse(tc.wantMemory)) != 0 {
				t.Fatalf("memory limit = %s, want %s", memory.String(), tc.wantMemory)
			}
		})
	}
}
