package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/coordination"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/decomposition"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/secretstore"
	"github.com/goobers/goobers/providers"
)

const coordinateHelp = "Usage: goobers coordinate --gaggle NAME --plan FILE [--check]\n" +
	"       [--evidence FILE] [--artifact FILE] [--result FILE] [instance-root]\n\n" +
	"Reconcile one explicitly approved cross-repository plan. Local operator-only;\n" +
	"never starts a run/daemon, implements code, merges, or deploys. Requires\n" +
	"instance.yaml coordination authority. --check validates and prints canonical\n" +
	"plan/evidence digests without resolving credentials or contacting providers.\n" +
	"Exit codes: 0 = checked or reconciled, 1 = refusal/provider failure, 2 = usage.\n" +
	"A successful pass may still report waiting, blocked, or integration-required.\n"

func runCoordinate(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("coordinate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "coordinate")
	gaggle := fs.String("gaggle", "", "owning configured gaggle")
	planPath := fs.String("plan", "", "maintainer-reviewed JSON plan")
	evidencePath := fs.String("evidence", "", "maintainer-reviewed JSON evidence")
	artifactPath := fs.String("artifact", "", "local integration log whose SHA-256 is attested")
	resultPath := fs.String("result", "", "JSON result file (also printed to stdout)")
	check := fs.Bool("check", false, "offline validation and canonical approval digests only")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *gaggle == "" || *planPath == "" || fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	if err := coordinateLocalOnly(); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	root := "."
	if fs.NArg() == 1 {
		root = fs.Arg(0)
	}
	layout := instance.Layout{Root: root}
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	var authority *coordination.Authority
	if cfg.Coordination != nil {
		for i := range cfg.Coordination.Gaggles {
			if cfg.Coordination.Gaggles[i].Name == *gaggle {
				authority = &cfg.Coordination.Gaggles[i]
			}
		}
	}
	if authority == nil {
		pln(stderr, "error: gaggle has no explicit coordination authority")
		return 1
	}
	var plan coordination.Plan
	if err := readCoordinateJSON(*planPath, &plan); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	if err := plan.Validate(*authority, !*check); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	var evidence *coordination.Evidence
	if *evidencePath != "" {
		evidence = &coordination.Evidence{}
		if err := readCoordinateJSON(*evidencePath, evidence); err != nil {
			pf(stderr, "error: %v\n", err)
			return 1
		}
	}
	planDigest, _ := coordination.Digest(plan)
	if *check {
		out := map[string]any{"planDigest": planDigest, "publications": coordination.Publications(plan)}
		if evidence != nil {
			if evidence.PlanDigest != planDigest {
				pln(stderr, "error: evidence planDigest does not match plan")
				return 1
			}
			out["evidenceDigest"], _ = coordination.Digest(evidence)
		}
		if err := json.NewEncoder(stdout).Encode(out); err != nil {
			pf(stderr, "error: %v\n", err)
			return 1
		}
		return 0
	}
	// Resolve the existing config tree without starting its scheduler.
	set, report, err := instance.LoadConfigDir(layout.ConfigDir())
	if err != nil {
		pf(stderr, "error: load gaggle definitions: %v\n", err)
		return 1
	}
	if report.HasErrors() {
		pln(stderr, "error: gaggle definitions failed validation")
		return 1
	}
	if err := coordinateGaggles(set, *authority); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	registry, _ := journal.DefaultScrubber()
	stores, err := secretstore.NewRegistry(cfg.SecretStores)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	reconciler := coordination.Reconciler{
		Authority: *authority, Providers: map[string]coordination.Provider{},
		Leaser: decomposition.FileTargetLeaser{Directory: filepath.Join(layout.Root, "coordination-locks")},
	}
	repos := []coordination.Repository{authority.ParentRepo}
	for _, child := range plan.Children {
		repos = append(repos, child.Repository)
	}
	for _, repo := range repos {
		if reconciler.Providers[repo.Key()] != nil {
			continue
		}
		provider, err := coordinateProvider(ctx, cfg, repo.Ref(), stores, registry)
		if err != nil {
			pf(stderr, "error: %v\n", scrubTerminalError(registry, err))
			return 1
		}
		reconciler.Providers[repo.Key()] = provider
	}
	if *artifactPath != "" {
		f, err := os.Open(*artifactPath)
		if err != nil {
			pf(stderr, "error: open integration artifact: %v\n", err)
			return 1
		}
		h := sha256.New()
		_, copyErr := io.Copy(h, f)
		closeErr := f.Close()
		if copyErr != nil || closeErr != nil {
			pf(stderr, "error: hash artifact: %v; close: %v\n", copyErr, closeErr)
			return 1
		}
		reconciler.ArtifactDigest = hex.EncodeToString(h.Sum(nil))
	}
	out, reconcileErr := reconciler.Reconcile(ctx, plan, evidence)
	if reconcileErr != nil {
		out.State, out.Reason = "error", scrubTerminalError(registry, reconcileErr).Error()
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	if *resultPath != "" {
		if err := os.WriteFile(*resultPath, data, 0o600); err != nil {
			pf(stderr, "error: write result: %v\n", err)
			return 1
		}
	}
	if _, err := fmt.Fprintln(stdout, string(data)); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	if reconcileErr != nil {
		pf(stderr, "error: %s\n", out.Reason)
		return 1
	}
	return 0
}

func coordinateGaggles(set *instance.ConfigSet, authority coordination.Authority) error {
	found := false
	owners := map[string][]string{}
	for _, g := range set.Gaggles {
		ref := providers.RepositoryRef{Provider: providers.ProviderKind(g.Spec.Project.Provider), Owner: g.Spec.Project.Owner, Project: g.Spec.Project.Project, Name: g.Spec.Project.Name, URL: g.Spec.Project.BaseURL}
		owners[ref.CanonicalKey()] = append(owners[ref.CanonicalKey()], g.Name)
		if g.Name == authority.Name && ref.CanonicalKey() == authority.ParentRepo.Key() {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("coordination owner must be a loaded gaggle for the exact parent repository")
	}
	for _, target := range authority.Targets {
		names := owners[target.Repository.Key()]
		if len(names) != 1 || names[0] == authority.Name {
			return fmt.Errorf("target %s/%s requires exactly one separate repo-owning gaggle", target.Repository.Owner, target.Repository.Name)
		}
	}
	return nil
}

func coordinateLocalOnly() error {
	for _, key := range []string{"GOOBERS_RUN_ID", "GOOBERS_WORKFLOW", "GOOBERS_PROVIDER_SNAPSHOT", "GOOBERS_CLAIMS_ENDPOINT", "KUBERNETES_SERVICE_HOST"} {
		if os.Getenv(key) != "" {
			return fmt.Errorf("coordinate is local operator-only; workflow stages, pods and tier-3 execution are unsupported (%s)", key)
		}
	}
	return nil
}

func readCoordinateJSON(path string, value any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return coordination.Decode(file, value)
}

// No credentials/daemonIdentity overrides: coordination is authorized only by
// the exact repository's own token binding, not a broad instance credential.
func coordinateProvider(ctx context.Context, cfg *instance.Config, repo providers.RepositoryRef, stores credentials.StoreResolver, registrar terminalSecretRegistry) (*providers.GitHubProvider, error) {
	scoped := *cfg
	scoped.Repos, scoped.Credentials, scoped.DaemonIdentity = nil, nil, nil
	for _, candidate := range cfg.Repos {
		ref := providers.RepositoryRef{Provider: providers.ProviderKind(candidate.Provider), Owner: candidate.Owner, Project: candidate.Project, Name: candidate.Name, URL: candidate.BaseURL}
		if ref.CanonicalKey() == repo.CanonicalKey() {
			scoped.Repos = append(scoped.Repos, candidate)
		}
	}
	if repo.Provider != providers.ProviderGitHub || len(scoped.Repos) != 1 {
		return nil, fmt.Errorf("coordination requires one exact configured GitHub repository")
	}
	resolver, _, err := buildCredentials(&scoped, stores, repo.Owner, repo.Name, nil, registrar)
	if err != nil {
		return nil, err
	}
	key := string(capability.CoordinationWrite) + "@" + repo.CanonicalKey()
	injector, err := credentials.NewInjector(resolver, []credentials.Grant{{Capability: key, Ref: repo.Owner + "/" + repo.Name}}, registrar)
	if err != nil {
		return nil, err
	}
	set, err := injector.Materialize(ctx, []string{key})
	if err != nil {
		return nil, err
	}
	if _, err := set.Token(ctx, key); err != nil {
		return nil, err
	}
	provider := providers.NewGitHubProvider("", providers.WithTokenSource(set.For(key)))
	if repo.URL != "" {
		provider.BaseURL = repo.URL
	}
	return provider, nil
}
