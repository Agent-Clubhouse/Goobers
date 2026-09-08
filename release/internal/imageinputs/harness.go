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
		pkg, p.version = "@github/copilot-linux-x64", "1.0.80"
		p.digest = "aafd72b553700372032bb91c428c3e7c08a40faeede36f8043c5f8d9b2bfee8b9d362bf87eb55930ed328750d821e6d545c9848663ef6d53d8fe9b5550addc6c"
	case "copilot/arm64":
		pkg, p.version = "@github/copilot-linux-arm64", "1.0.80"
		p.digest = "f285f037696ec871232284a4f009004c18d70e146842dba252f0bddc6a5f40a14723e2357a8395c08b00ab97ca3fc8cd085dab3ab60521c4e3c2646ddfc90271"
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
