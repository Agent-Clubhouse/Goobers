package main

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/agentickit"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/gooberassets"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/podauth"
	"github.com/goobers/goobers/internal/secretstore"
)

// agentickitwriter.go is the worker half of the agentic claim check.
//
// The worker has the instance configuration; a stage pod does not, and must
// not — that is the same property that stops a stage reading the fleet's
// config. So the worker resolves ONE goober's execution inputs, publishes them
// to the blob plane the pod already reaches, and the dispatcher stamps only the
// resulting digest.
//
// It re-derives from the instance root rather than extending buildRunnerConfig's
// return signature: that function is shared with the daemon, and widening a
// shared constructor to serve one caller is how the two paths start to drift.

// agenticKitWriter satisfies dispatcher.KitWriter.
type agenticKitWriter struct {
	instanceRoot string
	blobEndpoint string
	// minter issues the per-run bearer this writer presents to the blob plane.
	//
	// The worker holds the signing KEY, never a pod token: a pod token is
	// scoped to one run, and the worker serves many. Reading GOOBERS_POD_TOKEN
	// here — which is what the first version did — sends an empty Authorization
	// header and the plane answers 401. That is the same mistake #3726 fixed
	// for live-journal emission, in the same process, for the same reason.
	minter    *podauth.SignedKey
	registrar *journal.RegistryScrubber
}

// kitTokenTTL bounds a minted publish bearer. Short because it is used
// immediately by the PUT that mints it and never stored.
const kitTokenTTL = 5 * time.Minute

// WriteKit resolves the stage's goober, publishes its kit, and returns the
// content address to stamp on the pod.
func (w agenticKitWriter) WriteKit(ctx context.Context, attempt dispatcher.Attempt) (string, error) {
	if w.blobEndpoint == "" {
		return "", fmt.Errorf("agentic kit writer has no blob endpoint")
	}
	if attempt.Envelope == nil {
		return "", fmt.Errorf("agentic attempt for run %s stage %s carries no envelope", attempt.RunID, attempt.Stage)
	}
	env := *attempt.Envelope
	if env.Goober == "" {
		return "", fmt.Errorf("envelope for run %s stage %s names no goober", env.RunID, env.TaskID)
	}

	kit, err := w.buildKit(env)
	if err != nil {
		return "", err
	}
	data, digest, err := agentickit.Marshal(kit)
	if err != nil {
		return "", err
	}
	// Idempotent by content address: a retried attempt republishing an
	// identical kit is a no-op rather than a conflict.
	// Minted per run, scoped to the run being published for — never an ambient
	// credential, and never one run's authority used to publish another's.
	var token string
	if w.minter != nil {
		token, err = w.minter.Mint(attempt.RunID, kitTokenTTL)
		if err != nil {
			return "", fmt.Errorf("mint publish bearer for run %s: %w", attempt.RunID, err)
		}
	}
	blobs := &dispatcher.BlobClient{BaseURL: w.blobEndpoint, Token: token}
	if err := blobs.Put(ctx, digest, data); err != nil {
		return "", fmt.Errorf("publish agentic kit: %w", err)
	}
	return digest, nil
}

func (w agenticKitWriter) buildKit(env apiv1.InvocationEnvelope) (*agentickit.Kit, error) {
	l := instance.NewLayout(w.instanceRoot)
	cfg, err := instance.LoadConfig(l.ConfigFile())
	if err != nil {
		return nil, fmt.Errorf("load instance config: %w", err)
	}
	set, report, err := loadConfigDirectory(l.ConfigDir())
	if err != nil {
		if issues := validationIssueSummary(report); issues != "" {
			return nil, fmt.Errorf("load config directory: %w (%s)", err, issues)
		}
		return nil, fmt.Errorf("load config directory: %w", err)
	}
	instance.ApplyGaggleCICommand(set)
	instance.ApplyGaggleOutboxMirror(set)

	goobers, err := resolveGoobersForGaggle(set, env.Gaggle)
	if err != nil {
		return nil, err
	}
	// SCOPED TO THE ONE GOOBER THIS STAGE RUNS AS. A kit carrying every
	// goober's instructions would hand a stage the definitions of agents it has
	// no business seeing, and would grow with the gaggle rather than with the
	// stage.
	spec, ok := goobers[env.Goober]
	if !ok {
		return nil, fmt.Errorf("goober %q is not defined in gaggle %q", env.Goober, env.Gaggle)
	}
	scoped := map[string]apiv1.GooberSpec{env.Goober: spec}

	instructions, err := loadGooberInstructions(l.ConfigDir(), scoped)
	if err != nil {
		return nil, fmt.Errorf("load goober instructions: %w", err)
	}
	if _, ok := instructions[env.Goober]; !ok {
		return nil, fmt.Errorf("goober %q has no resolved instructions", env.Goober)
	}

	assets := make(map[string]*gooberassets.WireBundle, 1)
	bundle, err := gooberassets.Load(filepath.Join(gooberDefinitionDir(l.ConfigDir(), spec, env.Goober), gooberassets.SourceDir))
	if err != nil {
		return nil, fmt.Errorf("load goober %q assets: %w", env.Goober, err)
	}
	assets[env.Goober] = bundle.ToWire()

	stores, err := secretstore.NewRegistry(cfg.SecretStores)
	if err != nil {
		return nil, fmt.Errorf("resolve credential stores: %w", err)
	}
	project := gaggleProjectRef(set, env.Gaggle)
	_, grants, err := buildCredentials(cfg, stores, project.Owner, project.Name, nil, w.registrar)
	if err != nil {
		return nil, fmt.Errorf("derive credential grants: %w", err)
	}
	wireGrants := make([]agentickit.Grant, 0, len(grants))
	for _, g := range grants {
		// Shape only — Ref names where a credential lives, never its value.
		wireGrants = append(wireGrants, agentickit.Grant{Goober: g.Goober, Capability: g.Capability, Ref: g.Ref})
	}

	return &agentickit.Kit{
		Envelope:        env,
		Goobers:         scoped,
		Instructions:    instructions,
		Assets:          assets,
		EnvCapabilities: buildEnvCapabilities(),
		Grants:          wireGrants,
		SandboxPosture:  string(instance.EffectiveAgenticSandbox(cfg, nil)),
	}, nil
}
