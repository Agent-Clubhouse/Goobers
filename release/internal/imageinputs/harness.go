// Package imageinputs prepares verified inputs for release-owned image builds.
// It is tooling-only: importing it from product runtime is prohibited by Go's
// internal package boundary beneath release/.
package imageinputs

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

type harnessPin struct {
	name, version, url, digest string
}

func pinnedHarness(name, arch string) (harnessPin, error) {
	var p harnessPin
	p.name = name
	var pkg string
	switch name + "/" + arch {
	case "copilot/amd64":
		pkg, p.version = "@github/copilot-linux-x64", "1.0.83"
		p.digest = "1b8a5ee7fe1ac6350b305a49054eeb6a7ebdfbeed1227e709459fc5cc857685102075f1c3d539f53b07dc60a0215b1c61404f89004e5e3915af5db6c84a50d9a"
	case "copilot/arm64":
		pkg, p.version = "@github/copilot-linux-arm64", "1.0.83"
		p.digest = "a60552eb4797d66549f0c168f520ec6a53365f911acb0abafeb58983aae81c463818736e231deb1c521d9e841f6dc20772e198a47b90ff005b5a24a030a74d11"
	case "claude/amd64":
		pkg, p.version = "@anthropic-ai/claude-code-linux-x64", "2.1.263"
		p.digest = "d08aefa4b77fd053f469c430e7d4f80afc7faf89f0017281ad6d16ca1217ca6cd85aeacf08bced07e21cbbd32479ec00508fa9dcf4d7b127e68a5bff9fa5e501"
	case "claude/arm64":
		pkg, p.version = "@anthropic-ai/claude-code-linux-arm64", "2.1.263"
		p.digest = "46526d2db97cc6a14c7f6cd474e283e28e4590d85c8068e54b7527f0f35f49bbf57cb420dd2a8142010984d12d49e2a512c3acdba467086fe3564e326a1f1477"
	default:
		return p, fmt.Errorf("unsupported Linux harness %s/%s", name, arch)
	}
	p.url = "https://registry.npmjs.org/" + pkg + "/-/" + filepath.Base(pkg) + "-" + p.version + ".tgz"
	return p, nil
}

func harnessClient() *http.Client {
	return &http.Client{
		Timeout: 5 * time.Minute,
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			if len(via) >= 10 || r.URL.Scheme != "https" || r.URL.User != nil {
				return fmt.Errorf("harness dependency redirect must remain bounded HTTPS without credentials")
			}
			return nil
		},
	}
}

// PrepareHarness downloads one pinned Linux native package and prepares its
// runtime files, fixed launcher and provenance in a new directory. A failed
// download, checksum or extraction leaves no partial destination. The caller
// supplies the release's Dockerfile and base image; this function never runs a
// container engine, publishes an image or reads credentials.
func PrepareHarness(ctx context.Context, name, arch, directory string) error {
	pin, err := pinnedHarness(name, arch)
	if err != nil {
		return err
	}
	return prepareHarness(ctx, harnessClient(), pin, directory)
}

func prepareHarness(ctx context.Context, client *http.Client, pin harnessPin, directory string) error {
	if _, err := os.Lstat(directory); !os.IsNotExist(err) {
		return fmt.Errorf("harness context destination must not exist")
	}
	parent := filepath.Dir(directory)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(parent, ".harness-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(staging) }()
	archive := filepath.Join(staging, "harness.tgz")
	if err := downloadHarness(ctx, client, pin, archive, maxArchiveBytes); err != nil {
		return err
	}
	if err := unpackHarness(archive, filepath.Join(staging, "package"), pin.name, maxPackageBytes, maxPackageFiles); err != nil {
		return err
	}
	if err := writeLauncher(staging, pin); err != nil {
		return err
	}
	if err := os.Remove(archive); err != nil {
		return err
	}
	return os.Rename(staging, directory)
}

func writeLauncher(root string, pin harnessPin) error {
	var launcher string
	switch pin.name {
	case "copilot":
		// Loading the full immutable package avoids extracting native addons into
		// a noexec HOME. Set these here: default-deny stage env can drop image env.
		launcher = "#!/bin/sh\nCOPILOT_AUTO_UPDATE=false COPILOT_CLI_DIST_DIR=/opt/goobers-harness exec /opt/goobers-harness/copilot \"$@\"\n"
	case "claude":
		launcher = "#!/bin/sh\nDISABLE_AUTOUPDATER=1 exec /opt/goobers-harness/claude \"$@\"\n"
	default:
		return fmt.Errorf("unsupported harness launcher")
	}
	if err := os.Mkdir(filepath.Join(root, "bin"), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(root, "bin", pin.name), []byte(launcher), 0o555); err != nil {
		return err
	}
	for name, contents := range map[string]string{
		"version": pin.version + "\n",
		"source":  pin.url + "\nsha512:" + pin.digest + "\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(contents), 0o444); err != nil {
			return err
		}
	}
	return nil
}
