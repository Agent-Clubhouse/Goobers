package dispatcher

import (
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	"github.com/goobers/goobers/internal/runnercap"
)

// windows_identity_test.go pins the Windows container-identity binding
// (#3619): ContainerAdministrator is stamped exactly when the stage REQUIRES
// runnercap.CapabilityWindowsAdmin AND the resolved runner class PROVIDES it;
// every other Windows pod is stamped ContainerUser explicitly; a stage that
// requires the privilege on a class that lacks it (or on a non-Windows
// runner) is refused at render, never defaulted.

func windowsAdminRunner() RunnerSpec {
	runner := windowsRunner()
	runner.Name = "win-admin"
	runner.Restrictions = []string{"tmp:ephemeral"}
	runner.Capabilities = []string{"dotnet@8", runnercap.CapabilityWindowsAdmin}
	return runner
}

func adminAttempt() Attempt {
	attempt := testAttempt()
	attempt.RunsOnCapabilities = []string{"dotnet@8", runnercap.CapabilityWindowsAdmin}
	return attempt
}

func TestRenderPodWindowsAdminOnlyWhenRequiredAndProvided(t *testing.T) {
	t.Run("required and provided → ContainerAdministrator", func(t *testing.T) {
		pod, err := RenderPod(testConfig(), adminAttempt(), windowsAdminRunner())
		if err != nil {
			t.Fatalf("RenderPod: %v", err)
		}
		if got := windowsIdentity(t, pod); got != WindowsAdminRunAsUserName {
			t.Fatalf("runAsUserName = %q, want %q", got, WindowsAdminRunAsUserName)
		}
		// Admin is an identity, not a licence to skip the Windows bindings:
		// readOnlyRootFilesystem still absent, Linux baseline still absent.
		if c := pod.Spec.Containers[0]; c.SecurityContext.ReadOnlyRootFilesystem != nil ||
			c.SecurityContext.AllowPrivilegeEscalation != nil || c.SecurityContext.Capabilities != nil {
			t.Fatalf("Linux-only container securityContext fields stamped on an admin Windows pod: %+v", c.SecurityContext)
		}
	})

	t.Run("provided but not required → ContainerUser", func(t *testing.T) {
		attempt := testAttempt()
		attempt.RunsOnCapabilities = []string{"dotnet@8"}
		pod, err := RenderPod(testConfig(), attempt, windowsAdminRunner())
		if err != nil {
			t.Fatalf("RenderPod: %v", err)
		}
		if got := windowsIdentity(t, pod); got != WindowsRunAsUserName {
			t.Fatalf("runAsUserName = %q, want %q: a class that PROVIDES admin still runs a stage that did not ask for it as ContainerUser (least privilege in both directions)", got, WindowsRunAsUserName)
		}
	})

	t.Run("required but not provided → refused", func(t *testing.T) {
		runner := windowsRunner()
		runner.Restrictions = []string{"tmp:ephemeral"}
		runner.Capabilities = []string{"dotnet@8"}
		_, err := RenderPod(testConfig(), adminAttempt(), runner)
		var identity *WindowsIdentityError
		if !errors.As(err, &identity) {
			t.Fatalf("RenderPod err = %v, want WindowsIdentityError", err)
		}
		if identity.Runner != runner.Name || !strings.Contains(err.Error(), `requires "privilege=windows-admin" but the runner's provides.capabilities does not claim it`) {
			t.Fatalf("RenderPod err = %v, want the required-but-not-provided refusal naming the runner", err)
		}
	})

	t.Run("required on a linux runner → refused", func(t *testing.T) {
		_, err := RenderPod(testConfig(), adminAttempt(), linuxRunner())
		var identity *WindowsIdentityError
		if !errors.As(err, &identity) || !strings.Contains(err.Error(), `but the runner's os is "linux"`) {
			t.Fatalf("RenderPod err = %v, want WindowsIdentityError naming the runner's os", err)
		}
	})

	t.Run("linux identity model untouched", func(t *testing.T) {
		attempt := testAttempt()
		attempt.RunsOnCapabilities = []string{"go@1.26"}
		pod, err := RenderPod(testConfig(), attempt, linuxRunner())
		if err != nil {
			t.Fatalf("RenderPod: %v", err)
		}
		if sc := pod.Spec.SecurityContext; sc.WindowsOptions != nil {
			t.Fatalf("windowsOptions stamped on a Linux pod: %+v", sc.WindowsOptions)
		}
		if sc := pod.Spec.SecurityContext; sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
			t.Fatal("the Linux PSS-restricted baseline is gone")
		}
	})
}

// The template path decides the identity the same way, and the dispatcher's
// decision WINS over anything the consumer template pre-set — at the pod level
// and at the stage container level, because a container-level runAsUserName
// overrides the pod-level one in Kubernetes. A template whose stage container
// pre-set ContainerAdministrator must not run a non-admin stage as admin.
func TestRenderFromTemplateWindowsIdentityOverridesTemplate(t *testing.T) {
	deployment := testDeployment()
	deployment.Spec.Template.Spec.SecurityContext = &corev1.PodSecurityContext{
		WindowsOptions: &corev1.WindowsSecurityContextOptions{RunAsUserName: ptr.To(WindowsAdminRunAsUserName)},
	}
	deployment.Spec.Template.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{
		WindowsOptions: &corev1.WindowsSecurityContextOptions{RunAsUserName: ptr.To(WindowsAdminRunAsUserName)},
	}
	runner := windowsAdminRunner()
	runner.HostKind = "deployment"
	runner.Host = "consumer-template"

	attempt := testAttempt()
	attempt.RunsOnCapabilities = []string{"dotnet@8"}
	pod, err := RenderFromTemplate(testConfig(), attempt, runner, deployment)
	if err != nil {
		t.Fatalf("RenderFromTemplate: %v", err)
	}
	if got := windowsIdentity(t, pod); got != WindowsRunAsUserName {
		t.Fatalf("runAsUserName = %q, want %q: the template's ContainerAdministrator must not survive for a stage that did not require it", got, WindowsRunAsUserName)
	}

	pod, err = RenderFromTemplate(testConfig(), adminAttempt(), runner, deployment)
	if err != nil {
		t.Fatalf("RenderFromTemplate (admin): %v", err)
	}
	if got := windowsIdentity(t, pod); got != WindowsAdminRunAsUserName {
		t.Fatalf("runAsUserName = %q, want %q for a required-and-provided admin stage", got, WindowsAdminRunAsUserName)
	}

	// And the refusal arm holds on the template path too.
	runner.Capabilities = nil
	_, err = RenderFromTemplate(testConfig(), adminAttempt(), runner, deployment)
	var identity *WindowsIdentityError
	if !errors.As(err, &identity) {
		t.Fatalf("RenderFromTemplate err = %v, want WindowsIdentityError", err)
	}
}

// The identity decision is the STAGE's. A template's sidecars are operator-
// owned infrastructure (same trust root as instance.yaml): one that sets its
// own windowsOptions.runAsUserName keeps it — the container level wins in
// Kubernetes and the dispatcher does not rewrite it — while one that sets none
// inherits the pod-level stamp. Pinned so the boundary is a stated property
// of the rendered spec, not an accident of which containers stampSecurity
// happens to touch (the Linux arm draws the same line: PSS baseline on the
// stage container only).
func TestRenderFromTemplateWindowsSidecarsKeepTheirOwnIdentity(t *testing.T) {
	deployment := testDeployment()
	deployment.Spec.Template.Spec.Containers = append(deployment.Spec.Template.Spec.Containers,
		corev1.Container{
			Name: "log-shipper", Image: "ghcr.io/example/shipper:v1",
			SecurityContext: &corev1.SecurityContext{
				WindowsOptions: &corev1.WindowsSecurityContextOptions{RunAsUserName: ptr.To(WindowsAdminRunAsUserName)},
			},
		},
		corev1.Container{Name: "metrics", Image: "ghcr.io/example/metrics:v1"},
	)
	runner := windowsAdminRunner()
	runner.HostKind = "deployment"
	runner.Host = "consumer-template"

	attempt := testAttempt()
	attempt.RunsOnCapabilities = []string{"dotnet@8"}
	pod, err := RenderFromTemplate(testConfig(), attempt, runner, deployment)
	if err != nil {
		t.Fatalf("RenderFromTemplate: %v", err)
	}
	if got := windowsIdentity(t, pod); got != WindowsRunAsUserName {
		t.Fatalf("stage runAsUserName = %q, want %q", got, WindowsRunAsUserName)
	}
	if len(pod.Spec.Containers) != 3 {
		t.Fatalf("containers = %d, want the stage plus both template sidecars", len(pod.Spec.Containers))
	}
	shipper := pod.Spec.Containers[1].SecurityContext
	if shipper == nil || shipper.WindowsOptions == nil || shipper.WindowsOptions.RunAsUserName == nil ||
		*shipper.WindowsOptions.RunAsUserName != WindowsAdminRunAsUserName {
		t.Fatalf("sidecar %q securityContext = %+v, want its own operator-set %s kept", pod.Spec.Containers[1].Name, shipper, WindowsAdminRunAsUserName)
	}
	if metrics := pod.Spec.Containers[2].SecurityContext; metrics != nil && metrics.WindowsOptions != nil && metrics.WindowsOptions.RunAsUserName != nil {
		t.Fatalf("sidecar %q got a container-level runAsUserName %q; it should inherit the pod-level stamp", pod.Spec.Containers[2].Name, *metrics.WindowsOptions.RunAsUserName)
	}
}
