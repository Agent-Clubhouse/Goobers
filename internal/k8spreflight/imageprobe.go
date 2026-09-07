package k8spreflight

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/platform/proc"
)

type overlayCommand struct {
	Program string
	Args    []string
	Input   []byte
}

type overlayCommandRunner func(context.Context, overlayCommand) ([]byte, error)

type imageRequirements struct {
	Image, Commit   string
	Tools           []string
	Executables     []string
	RootCA          []byte
	MinimumCommit   string
	PullPolicy      string
	ScriptVariables []string
}

type imageObservation struct {
	ImageID   string
	Checked   int
	Unchecked []string
}

type probeBuffer struct {
	bytes.Buffer
	truncated bool
}

func (b *probeBuffer) Write(data []byte) (int, error) {
	const limit = 4 << 20
	retained := min(len(data), limit-b.Len())
	_, _ = b.Buffer.Write(data[:retained])
	b.truncated = b.truncated || retained < len(data)
	return len(data), nil
}

// executeOverlayCommand bounds output and kills the complete host-side process
// tree on cancellation. Stderr is diagnostic output, never protocol input.
func executeOverlayCommand(ctx context.Context, request overlayCommand) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cmd := exec.Command(request.Program, request.Args...)
	cmd.Stdin = bytes.NewReader(request.Input)
	cmd.WaitDelay = time.Second
	var stdout, stderr probeBuffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	tree, err := proc.Start(cmd)
	if err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err = <-done:
	case <-ctx.Done():
		_ = tree.Kill()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
		return nil, ctx.Err() // Do not read buffers while a pipe could still write.
	}
	if err != nil {
		return nil, fmt.Errorf("%s probe failed: %w", request.Program, err)
	}
	if stdout.truncated || stderr.truncated {
		return nil, fmt.Errorf("%s probe exceeded the 4 MiB output bound", request.Program)
	}
	return stdout.Bytes(), nil
}

var imageReferencePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9./:_@-]*$`)
var imageIDPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
var imageToolPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.+-]*$`)
var imageExecutablePattern = regexp.MustCompile(`^[A-Za-z0-9_/][A-Za-z0-9_./:\\+-]*$`)

func probePinnedImage(ctx context.Context, run overlayCommandRunner, runtime string, required imageRequirements) (imageObservation, error) {
	var observation imageObservation
	if !imageReferencePattern.MatchString(required.Image) || len(required.Commit) != 40 || !overlayCommit.MatchString(required.Commit) {
		return observation, fmt.Errorf("image contract requires a concrete image reference and full commit")
	}
	if required.MinimumCommit != "" {
		if err := probeImageSourceRequirement(ctx, run, required); err != nil {
			return observation, err
		}
		observation.Checked++
	}
	data, err := acquirePinnedImage(ctx, run, runtime, required)
	if err != nil {
		return observation, err
	}
	var inspected []struct{ ID, OS string }
	if err := json.Unmarshal(data, &inspected); err != nil || len(inspected) != 1 || !imageIDPattern.MatchString(inspected[0].ID) {
		return observation, fmt.Errorf("runtime did not identify exactly one immutable image")
	}
	observation.ImageID = inspected[0].ID
	imageOS := strings.ToLower(inspected[0].OS)
	if imageOS != "linux" && imageOS != "windows" {
		return observation, fmt.Errorf("unsupported image OS %q; contents not checked", imageOS)
	}
	invoke := func(program string, args []string, input []byte) ([]byte, error) {
		image := imageRuntime{run: run, runtime: runtime, id: observation.ImageID, os: imageOS}
		return image.invoke(ctx, program, args, input)
	}
	if err := probeImageVersion(invoke, required.Commit); err != nil {
		return observation, err
	}
	observation.Checked++
	if len(required.Tools) == 0 {
		observation.Unchecked = append(observation.Unchecked, "required PATH tools not supplied; PATH tool inventory unchecked")
	}
	for _, tool := range required.Tools {
		if !imageToolPattern.MatchString(tool) {
			return observation, fmt.Errorf("invalid required PATH tool %q", tool)
		}
		if err := probeImageTool(invoke, imageOS, tool, false); err != nil {
			return observation, err
		}
		observation.Checked++
	}
	for _, executable := range required.Executables {
		if !imageExecutablePattern.MatchString(executable) {
			return observation, fmt.Errorf("invalid required executable path")
		}
		if err := probeImageTool(invoke, imageOS, executable, true); err != nil {
			return observation, err
		}
		observation.Checked++
	}
	if err := probeImageScriptVariables(invoke, imageOS, required.ScriptVariables); err != nil {
		return observation, err
	}
	observation.Checked += len(required.ScriptVariables)
	if len(required.RootCA) == 0 {
		observation.Unchecked = append(observation.Unchecked, "internal root CA not supplied; trust anchor unchecked")
		return observation, nil
	}
	if err := probeImageCA(invoke, imageOS, required.RootCA); err != nil {
		return observation, err
	}
	observation.Checked++
	return observation, nil
}

type imageInvocation func(string, []string, []byte) ([]byte, error)

func acquirePinnedImage(ctx context.Context, run overlayCommandRunner, runtime string, required imageRequirements) ([]byte, error) {
	switch required.PullPolicy {
	case "", "always":
		if _, err := run(ctx, overlayCommand{Program: runtime, Args: []string{"pull", required.Image}}); err != nil {
			return nil, fmt.Errorf("pinned image unavailable: %w", err)
		}
	case "never":
		// Explicit local-artifact inspection, never a failed-pull fallback.
	default:
		return nil, fmt.Errorf("image pull policy must be always or never")
	}
	return run(ctx, overlayCommand{Program: runtime, Args: []string{"image", "inspect", required.Image}})
}

func probeImageVersion(invoke imageInvocation, commit string) error {
	data, err := invoke("goobers", []string{"--version", "--json"}, nil)
	if err != nil {
		return fmt.Errorf("goobers binary not executable in pinned image: %w", err)
	}
	var version struct{ Commit string }
	if err := json.Unmarshal(data, &version); err != nil || version.Commit != commit {
		return fmt.Errorf("image binary version stamp does not equal pin %s", commit)
	}
	return nil
}

type imageRuntime struct {
	run             overlayCommandRunner
	runtime, id, os string
}

func (image imageRuntime) invoke(ctx context.Context, program string, args []string, input []byte) ([]byte, error) {
	name := "goobers-doctor-" + strings.ToLower(rand.Text())
	command := []string{"run", "--name", name, "--network", "none", "-i"}
	if image.os == "linux" {
		command = append(command, "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges")
	}
	command = append(command, "--entrypoint", program, image.id)
	command = append(command, args...)
	output, invokeErr := image.run(ctx, overlayCommand{Program: image.runtime, Args: command, Input: input})
	// A dead Docker client does not imply a dead container. Remove only the
	// unique container this invocation named, even after caller cancellation.
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, cleanupErr := image.run(cleanupCtx, overlayCommand{Program: image.runtime, Args: []string{"rm", "--force", name}})
	if invokeErr != nil {
		return nil, invokeErr
	}
	if cleanupErr != nil {
		return nil, fmt.Errorf("remove probe container %s: %w", name, cleanupErr)
	}
	return output, nil
}

func probeImageTool(invoke imageInvocation, imageOS, tool string, absolute bool) error {
	program, args := "/bin/sh", []string{"-c", `command -v "$1" >/dev/null`, "probe", tool}
	if absolute {
		args[1] = `test -x "$1"`
	}
	if imageOS == "windows" {
		program = "powershell.exe"
		script := "$ErrorActionPreference='Stop'; Get-Command -CommandType Application -Name '" + tool + "' | Out-Null"
		if absolute {
			script = "if (-not (Test-Path -LiteralPath '" + tool + "' -PathType Leaf)) { exit 1 }"
		}
		args = []string{"-NoProfile", "-NonInteractive", "-Command", script}
	}
	if _, err := invoke(program, args, nil); err != nil {
		return fmt.Errorf("required image tool %s unavailable: %w", tool, err)
	}
	return nil
}

func probeImageCA(invoke imageInvocation, imageOS string, encoded []byte) error {
	block, rest := pem.Decode(encoded)
	if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		return fmt.Errorf("internal root CA must be exactly one PEM certificate")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil || !certificate.IsCA || certificate.CheckSignatureFrom(certificate) != nil {
		return fmt.Errorf("internal root CA must be a valid self-signed CA certificate")
	}
	if now := time.Now(); now.Before(certificate.NotBefore) || now.After(certificate.NotAfter) {
		return fmt.Errorf("internal root CA is outside its validity period")
	}
	if imageOS == "windows" {
		// SHA-1 here is the Windows certificate-store lookup key, not a trust
		// decision or signature algorithm. certutil must find this exact root.
		thumbprint := fmt.Sprintf("%X", sha1.Sum(certificate.Raw))
		_, err = invoke("certutil.exe", []string{"-store", "Root", thumbprint}, nil)
	} else {
		_, err = invoke("/bin/sh", []string{"-c", `set -eu; bundle="${SSL_CERT_FILE:-/etc/ssl/certs/ca-certificates.crt}"; exec openssl verify -CAfile "$bundle" /dev/stdin`}, encoded)
	}
	if err != nil {
		return fmt.Errorf("internal CA is not proven anchored in the image trust store: %w", err)
	}
	return nil
}
