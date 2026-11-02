// Package temporalops contains the read-only Temporal operator CLI surfaces.
package temporalops

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/safeio"
	"github.com/goobers/goobers/internal/signals"
	"github.com/goobers/goobers/internal/temporaldial"
	"github.com/goobers/goobers/internal/version"
	"github.com/goobers/goobers/internal/workercontract"
	"github.com/goobers/goobers/internal/workerrouting"
	"github.com/goobers/goobers/internal/workflow/ir"
)

// RolloutHelp and ContractHelp are shared by the command registry and parser.
const RolloutHelp = "Usage: goobers temporal rollout-status [--timeout 30s] [path]\n\n" +
	"Read native worker-deployment routing and per-version drainage as JSON. Uses\n" +
	"the instance engine endpoint, namespace and TLS configuration. The complete\n" +
	"inspection is bounded by --timeout (maximum 5m) and 128 versions. A routing\n" +
	"change or incomplete read fails without printing a misleading snapshot.\n\n" +
	"This does not change routing, identify last-known-good, prove worker readiness,\n" +
	"or authorize retiring a build. Drainage timestamps may be stale; pinned\n" +
	"histories, accepted starts and closed-history queries can still need a build.\n"

// ContractHelp describes the offline worker-contract inspection command.
const ContractHelp = "Usage: goobers temporal worker-contract [--workflow-ir file.json] [--allow-preview]\n\n" +
	"Emit this executable's digest and declared interpreter contracts as JSON.\n" +
	"Optionally check a canonical workflow IR (maximum 1 MiB) against this binary.\n" +
	"The IR must retain its exact source definition and match its normalization.\n" +
	"Preview support requires --allow-preview; this is an offline inspection flag.\n\n" +
	"Evidence only: no admission, remote-worker attestation, placement, credential\n" +
	"or readiness guarantee. The runtime still executes the DSL source. Effective\n" +
	"versions and build IDs are equality identities, not compatibility ranges.\n\n" +
	"Exit codes: 0 = manifest emitted / compatible source, 1 = inspection failed\n" +
	"or source incompatible/unverifiable, 2 = invalid flags or input file.\n"

// Commands accepts the host's flag-set constructor so completion audits observe
// these flags through the same registry as other commands.
type Commands struct {
	NewFlagSet func(string, flag.ErrorHandling) *flag.FlagSet
}

func (c Commands) flags(name string) *flag.FlagSet {
	if c.NewFlagSet != nil {
		return c.NewFlagSet(name, flag.ContinueOnError)
	}
	return flag.NewFlagSet(name, flag.ContinueOnError)
}

// RunRollout prints one bounded, coherent native deployment snapshot.
func (c Commands) RunRollout(args []string, stdout, stderr io.Writer) int {
	return runRollout(args, stdout, stderr, probeRollout, c.flags("temporal rollout-status"))
}

type rolloutProbe func(context.Context, *instance.Config) (workerrouting.RolloutSnapshot, error)

func probeRollout(ctx context.Context, cfg *instance.Config) (workerrouting.RolloutSnapshot, error) {
	engine := cfg.EffectiveEngineConfig()
	c, err := temporaldial.Dial(ctx, engine.HostPort, engine.Namespace, engine.TLS)
	if err != nil {
		return workerrouting.RolloutSnapshot{}, err
	}
	defer c.Close()
	return workerrouting.ReadRollout(ctx, c.WorkerDeploymentClient().GetHandle(workerrouting.DeploymentName), time.Now())
}

func runRollout(args []string, stdout, stderr io.Writer, probe rolloutProbe, fs *flag.FlagSet) int {
	fs.SetOutput(stderr)
	timeout := fs.Duration("timeout", 30*time.Second, "bound on the complete inspection")
	fs.Usage = func() { _, _ = io.WriteString(stderr, RolloutHelp) }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 || *timeout <= 0 || *timeout > 5*time.Minute {
		return fail(stderr, 2, errors.New("expected at most one instance path and timeout in (0,5m]"))
	}
	root := "."
	if fs.NArg() == 1 {
		root = fs.Arg(0)
	}
	cfg, err := instance.LoadConfig(instance.NewLayout(root).ConfigFile())
	if err != nil {
		return fail(stderr, 2, err)
	}
	ctx, stop := signals.SetupSignalContext()
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	result, err := probe(ctx, cfg)
	if err != nil {
		return fail(stderr, 1, err)
	}
	return emit(stdout, stderr, result)
}

// ContractReport explicitly separates this binary's declaration and an optional
// offline inspection. Neither field is consumed by runtime admission yet.
type ContractReport struct {
	Manifest       workercontract.Manifest    `json:"manifest"`
	ManifestDigest string                     `json:"manifestDigest"`
	Inspection     *workercontract.Inspection `json:"inspection,omitempty"`
	Limitation     string                     `json:"limitation"`
}

// RunContract never connects to Temporal or reads an instance configuration.
func (c Commands) RunContract(args []string, stdout, stderr io.Writer) int {
	return runContract(args, stdout, stderr, executableDigest, c.flags("temporal worker-contract"))
}

func runContract(args []string, stdout, stderr io.Writer, digestExecutable func() (string, error), fs *flag.FlagSet) int {
	fs.SetOutput(stderr)
	path := fs.String("workflow-ir", "", "canonical workflow IR JSON file")
	allowPreview := fs.Bool("allow-preview", false, "inspect with preview support enabled")
	fs.Usage = func() { _, _ = io.WriteString(stderr, ContractHelp) }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		return fail(stderr, 2, errors.New("unexpected positional arguments"))
	}
	var document ir.Document
	if *path != "" {
		if err := readIR(*path, &document); err != nil {
			return fail(stderr, 2, err)
		}
	}
	digest, err := digestExecutable()
	if err != nil {
		return fail(stderr, 1, err)
	}
	manifest, err := workercontract.Current(version.Get().Version, digest)
	if err != nil {
		return fail(stderr, 1, err)
	}
	manifestDigest, err := manifest.Digest()
	if err != nil {
		return fail(stderr, 1, err)
	}
	report := ContractReport{Manifest: manifest, ManifestDigest: manifestDigest,
		Limitation: "local declaration and on-disk executable-file digest only; not running-image attestation or admission enforcement; build ID uniqueness is not established"}
	if *path != "" {
		inspection := workercontract.Inspect(document, *allowPreview)
		report.Inspection = &inspection
	}
	if code := emit(stdout, stderr, report); code != 0 {
		return code
	}
	if report.Inspection != nil && report.Inspection.State != "compatible" {
		return 1
	}
	return 0
}

const maxIRBytes = 1 << 20

func readIR(path string, document *ir.Document) error {
	// The shared reader checks type before opening and uses nonblocking open
	// on Unix, so a FIFO (including a raced replacement) cannot hang the CLI.
	raw, err := safeio.ReadRegularInRoot(filepath.Dir(path), filepath.Base(path), maxIRBytes)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(document); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("workflow IR must contain exactly one JSON document")
	}
	return nil
}

func executableDigest() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	hash := sha256.New()
	if _, err = io.Copy(hash, f); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

func emit(stdout, stderr io.Writer, value any) int {
	if err := json.NewEncoder(stdout).Encode(value); err != nil {
		return fail(stderr, 1, err)
	}
	return 0
}

func fail(stderr io.Writer, code int, err error) int {
	_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
	return code
}
