package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const versionPkg = "github.com/goobers/goobers/internal/version"

// buildPackage is the main package the release binary is built from. A var, not
// a const, so tests can point the build at a small in-module package instead of
// cross-compiling the whole daemon.
var buildPackage = "./cmd/goobers"

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "release:", err)
		os.Exit(1)
	}
}

type options struct {
	version               string
	commit                string
	date                  string
	outDir                string
	targets               []Target
	checksums             bool
	skipUnbuildable       bool
	previousSupportMatrix string
}

func run(args []string, stdout, stderr io.Writer) error {
	opts, err := parseFlags(args, stderr)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(opts.outDir, 0o755); err != nil {
		return fmt.Errorf("create output dir %s: %w", opts.outDir, err)
	}

	ldflags := fmt.Sprintf("-s -w -X %s.Version=%s -X %s.Commit=%s -X %s.Date=%s",
		versionPkg, opts.version, versionPkg, opts.commit, versionPkg, opts.date)

	var archives []string
	var skipped []string
	for _, t := range opts.targets {
		binPath, buildOut, err := buildTarget(t, ldflags, opts.outDir)
		if err != nil {
			if opts.skipUnbuildable {
				_, _ = fmt.Fprintf(stdout, "skip  %-14s (does not compile yet)\n", t)
				skipped = append(skipped, t.String())
				continue
			}
			return fmt.Errorf("build %s failed — the release matrix requires every "+
				"target to compile (windows is gated on the #633 CI leg going green); "+
				"pass -skip-unbuildable to package only what builds:\n%s", t, buildOut)
		}
		archivePath, err := packageArchive(t, opts.version, binPath, opts.outDir)
		if err != nil {
			return err
		}
		_ = os.Remove(binPath) // keep only the archive
		archives = append(archives, archivePath)
		_, _ = fmt.Fprintf(stdout, "build %-14s -> %s\n", t, filepath.Base(archivePath))
	}

	if len(archives) > 0 {
		notesPath, snapshotPath, err := writeSupportReleaseMetadata(
			opts.version,
			opts.previousSupportMatrix,
			opts.outDir,
		)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "wrote %s and %s\n", filepath.Base(notesPath), filepath.Base(snapshotPath))
	}

	if opts.checksums && len(archives) > 0 {
		manifest, err := checksumsManifest(archives)
		if err != nil {
			return err
		}
		sumsPath := filepath.Join(opts.outDir, "SHA256SUMS")
		if err := os.WriteFile(sumsPath, []byte(manifest), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", sumsPath, err)
		}
		_, _ = fmt.Fprintf(stdout, "wrote %s (%d artifact(s))\n", filepath.Base(sumsPath), len(archives))
	}

	if len(skipped) > 0 {
		_, _ = fmt.Fprintf(stdout, "\nNOTE: skipped un-buildable target(s): %s\n"+
			"These are NOT in the release — a real tagged release must not skip a "+
			"required platform (that is the false-green trap #655's gate prevents).\n",
			strings.Join(skipped, ", "))
	}
	return nil
}

func parseFlags(args []string, stderr io.Writer) (options, error) {
	fs := flag.NewFlagSet("release", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		version         = fs.String("version", "", "release version (default: git describe --tags --always --dirty)")
		commit          = fs.String("commit", "", "build commit (default: git rev-parse --short HEAD)")
		date            = fs.String("date", "", "build date RFC3339 (default: the commit's committer date, for reproducibility)")
		outDir          = fs.String("output", "dist", "output directory for archives + SHA256SUMS")
		targetCSV       = fs.String("targets", "", "comma-separated os/arch list (default: the full release matrix)")
		checksums       = fs.Bool("checksums", true, "write a SHA256SUMS manifest over the archives")
		skip            = fs.Bool("skip-unbuildable", false, "package only targets that compile, skipping (not failing on) the rest")
		previousSupport = fs.String("previous-support-matrix", "", "previous release's dsl-support-matrix.json")
	)
	if err := fs.Parse(args); err != nil {
		return options{}, err
	}

	opts := options{
		outDir:                *outDir,
		checksums:             *checksums,
		skipUnbuildable:       *skip,
		previousSupportMatrix: *previousSupport,
	}

	opts.version = firstNonEmpty(*version, os.Getenv("GOOBERS_VERSION"), gitOutput("describe", "--tags", "--always", "--dirty"), "dev")
	opts.commit = firstNonEmpty(*commit, gitOutput("rev-parse", "--short", "HEAD"), "none")
	opts.date = firstNonEmpty(*date, gitOutput("show", "-s", "--format=%cI", "HEAD"), "unknown")

	targets, err := parseTargets(*targetCSV)
	if err != nil {
		return options{}, err
	}
	opts.targets = targets
	return opts, nil
}

// parseTargets turns a "windows/amd64,linux/arm64" string into Targets; an
// empty string yields DefaultTargets.
func parseTargets(csv string) ([]Target, error) {
	csv = strings.TrimSpace(csv)
	if csv == "" {
		return DefaultTargets, nil
	}
	var out []Target
	for _, tok := range strings.Split(csv, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		goos, goarch, ok := strings.Cut(tok, "/")
		if !ok || goos == "" || goarch == "" {
			return nil, fmt.Errorf("invalid target %q, want os/arch", tok)
		}
		out = append(out, Target{OS: goos, Arch: goarch})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no targets parsed from %q", csv)
	}
	return out, nil
}

// buildTarget cross-compiles ./cmd/goobers for t into outDir, returning the
// built binary path. -trimpath + injected metadata make the build reproducible
// and metadata-consistent with `make build`. Returns the combined build output
// on failure so the caller can surface a real cross-compile error (e.g. the
// missing windows internal/platform/proc impl) rather than a bare exit code.
func buildTarget(t Target, ldflags, outDir string) (binPath string, buildOutput string, err error) {
	binPath = filepath.Join(outDir, t.binaryName()+"."+t.OS+"-"+t.Arch)
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags", ldflags, "-o", binPath, buildPackage)
	cmd.Env = append(os.Environ(), "GOOS="+t.OS, "GOARCH="+t.Arch, "CGO_ENABLED=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", string(out), err
	}
	return binPath, "", nil
}

func gitOutput(args ...string) string {
	out, err := exec.Command("git", args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
