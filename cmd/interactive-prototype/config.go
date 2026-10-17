package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/interactive"
	"github.com/goobers/goobers/internal/interactiveops"
	"github.com/goobers/goobers/internal/secretpattern"
)

func operationsPolicy(surface interactive.Surface) interactiveops.Policy {
	capabilities := []interactiveops.CommandCapability{}
	for _, capability := range surface.ChildCommandCapabilities {
		capabilities = append(capabilities, interactiveops.CommandCapability(capability))
	}
	return interactiveops.Policy{Revision: surface.ConfigRevision, Gaggle: surface.Gaggle, Workflows: append([]string{}, surface.Workflows...), CommandCapabilities: capabilities, AllowAuthoring: slices.Contains(surface.Tools, "propose_action")}
}

// The catalog is a compiled projection of the ordinary config-as-code tree.
// It is never another authoring format. Private immutable snapshots keep a
// running session's definitions separate from later edits to the source tree.
type dslCatalog struct {
	Revision, ConfigDir string
	Surfaces            []interactive.Surface
	Authorities         map[string]map[string]apiv1.InteractiveAuthority
}

type catalogStore struct {
	mu       sync.RWMutex
	reloadMu sync.Mutex
	current  *dslCatalog
}

func (c *catalogStore) reload(source, stateDir string, service *interactive.Service) (string, error) {
	c.reloadMu.Lock()
	defer c.reloadMu.Unlock()
	next, err := loadDSLCatalog(source, stateDir)
	if err != nil {
		return "", err
	}
	// The coordinator pauses admission and confirms cancellation/authority
	// fencing before publishing new policies. Do not hold the catalog lock
	// while draining invocations, which may still be checking their old policy.
	if err := service.ReloadSurfaces(next.Surfaces); err != nil {
		return "", err
	}
	c.mu.Lock()
	c.current = next
	c.mu.Unlock()
	return next.Revision, nil
}

func (c *catalogStore) execution(req interactive.ExecutionRequest) (interactive.Surface, map[string]apiv1.InteractiveAuthority, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, surface := range c.current.Surfaces {
		if surface.ID == req.SurfaceID && surface.ConfigRevision == req.ConfigRevision && surface.Enabled {
			return surface, c.current.Authorities[surface.ID], nil
		}
	}
	return interactive.Surface{}, nil, fmt.Errorf("interactive DSL policy changed or was disabled; open a new session")
}

func (c *catalogStore) session(service *interactive.Service, owner, sessionID string) (interactive.Surface, string, error) {
	surface, err := service.SurfaceForSession(owner, sessionID)
	if err != nil {
		return interactive.Surface{}, "", err
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if surface.ConfigRevision != c.current.Revision {
		return interactive.Surface{}, "", fmt.Errorf("session DSL snapshot is retired")
	}
	return surface, c.current.ConfigDir, nil
}

func loadDSLCatalog(source, stateDir string) (*dslCatalog, error) {
	source, err := filepath.Abs(source)
	if err != nil {
		return nil, err
	}
	root := filepath.Join(stateDir, "config-snapshots")
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	stage, err := os.MkdirTemp(root, "loading-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(stage)
	configDir := filepath.Join(stage, "config")
	digest, err := snapshotConfigLayout(source, configDir)
	if err != nil {
		return nil, err
	}
	set, report, err := instance.LoadConfigDir(configDir)
	if err != nil {
		// Validation is local configuration data, never a credential-bearing
		// provider response. Still scrub diagnostics before returning them.
		diagnostics, _ := json.Marshal(report)
		return nil, fmt.Errorf("interactive config failed normal validation: %w: %s", err, secretpattern.NewScrubber().Scrub(diagnostics))
	}
	catalog, err := compileDSLCatalog(set, configDir, digest)
	if err != nil {
		return nil, err
	}
	final := filepath.Join(root, digest)
	if err := os.Rename(stage, final); err != nil {
		if info, statErr := os.Stat(filepath.Join(final, "config")); statErr != nil || !info.IsDir() {
			return nil, err
		}
		// Never trust a pre-existing snapshot solely because its directory name
		// matches. Re-hash before using it as the native execution source.
		check, err := configLayoutDigest(filepath.Join(final, "config"))
		if err != nil || check != digest {
			return nil, fmt.Errorf("stored DSL snapshot failed integrity validation")
		}
	}
	catalog.ConfigDir = filepath.Join(final, "config")
	return catalog, nil
}

func snapshotConfig(source, destination string) (string, error) {
	return walkConfig(source, func(relative string, data []byte) error {
		path := filepath.Join(destination, relative)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		return os.WriteFile(path, data, 0600)
	})
}

func withinPath(root, target string) bool {
	relative, err := filepath.Rel(root, target)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func snapshotConfigLayout(source, destination string) (string, error) {
	if _, err := snapshotConfig(source, destination); err != nil {
		return "", err
	}
	shared := filepath.Join(filepath.Dir(source), "goobers")
	if _, err := os.Lstat(shared); err == nil {
		if _, err := snapshotConfig(shared, filepath.Join(filepath.Dir(destination), "goobers")); err != nil {
			return "", err
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	return configLayoutDigest(destination)
}

func configLayoutDigest(configDir string) (string, error) {
	definitions, err := configDigest(configDir)
	if err != nil {
		return "", err
	}
	shared := "absent"
	sharedRoot := filepath.Join(filepath.Dir(configDir), "goobers")
	if _, err := os.Lstat(sharedRoot); err == nil {
		shared, err = configDigest(sharedRoot)
		if err != nil {
			return "", err
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	sum := sha256.Sum256([]byte("config:" + definitions + "\nshared-goobers:" + shared))
	return hex.EncodeToString(sum[:]), nil
}

func configDigest(source string) (string, error) { return walkConfig(source, nil) }

func walkConfig(source string, write func(string, []byte) error) (string, error) {
	hash := sha256.New()
	var total int64
	err := filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if strings.HasPrefix(entry.Name(), ".") && relative != "." {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("DSL snapshot refuses non-regular file %s", relative)
		}
		// This prototype materializes definitions and instruction documents.
		// Arbitrary static assets need the production asset materializer before
		// they can be enabled, rather than being silently inherited.
		if !slices.Contains([]string{".yaml", ".yml", ".md"}, strings.ToLower(filepath.Ext(path))) {
			return fmt.Errorf("interactive DSL snapshot does not support asset %s", relative)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		if info.Size() > 1<<20 || total > 16<<20 {
			return fmt.Errorf("interactive DSL snapshot exceeds bounded size")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !bytes.Equal(secretpattern.NewScrubber().Scrub(data), data) {
			return fmt.Errorf("DSL file %s contains credential-like content; use external broker custody", relative)
		}
		fmt.Fprintf(hash, "%d:%s:%d:", len(relative), relative, len(data))
		hash.Write(data)
		if write != nil {
			return write(relative, data)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func compileDSLCatalog(set *instance.ConfigSet, configDir, revision string) (*dslCatalog, error) {
	catalog := &dslCatalog{Revision: revision, Authorities: map[string]map[string]apiv1.InteractiveAuthority{}}
	goobers := map[string]apiv1.Goober{}
	for _, goober := range set.Goobers {
		goobers[goober.Name] = goober
	}
	for _, gaggle := range set.Gaggles {
		if gaggle.Spec.Interactive == nil || gaggle.Spec.Enabled != nil && !*gaggle.Spec.Enabled {
			continue
		}
		bindings := map[string]apiv1.InteractiveAuthority{}
		for _, authority := range gaggle.Spec.Interactive.Authorities {
			bindings[authority.Name] = authority
		}
		for _, declared := range gaggle.Spec.Interactive.Surfaces {
			goober, ok := goobers[declared.GooberRef]
			if !ok {
				return nil, fmt.Errorf("surface %s references unavailable Goober %s", declared.Name, declared.GooberRef)
			}
			if goober.Spec.Harness != apiv1.HarnessCodex {
				return nil, fmt.Errorf("surface %s: prototype adapter supports harness codex only", declared.Name)
			}
			if len(goober.Spec.Tools)+len(goober.Spec.MCPServers)+len(goober.Spec.HarnessOptions)+len(goober.Spec.Skills)+len(goober.Spec.Capabilities)+len(goober.Spec.PolicyActions)+len(goober.Spec.ConditionalPolicyActions) != 0 {
				return nil, fmt.Errorf("surface %s: Goober tools, external MCP, harness options, skills and capability grants are unsupported by this interactive adapter; no settings are silently inherited", declared.Name)
			}
			definition, ok := set.GooberSource(goober.Name)
			if !ok {
				return nil, fmt.Errorf("Goober source unavailable")
			}
			instructionsPath := filepath.Join(filepath.Dir(definition), goober.Spec.Instructions)
			if !withinPath(configDir, filepath.Join(configDir, instructionsPath)) && !withinPath(filepath.Join(filepath.Dir(configDir), "goobers"), filepath.Join(configDir, instructionsPath)) {
				return nil, fmt.Errorf("Goober instructions must remain within the snapshotted config or shared-goobers roots")
			}
			instructions, err := os.ReadFile(filepath.Join(configDir, instructionsPath))
			if err != nil {
				return nil, err
			}
			turnSeconds := int(declared.Session.MaxTurnSeconds)
			if goober.Spec.TimeoutSeconds > 0 && int(goober.Spec.TimeoutSeconds) < turnSeconds {
				turnSeconds = int(goober.Spec.TimeoutSeconds)
			}
			name := declared.DisplayName
			if name == "" {
				name = declared.Name
			}
			surface := interactive.Surface{ID: gaggle.Name + "--" + declared.Name, Name: name, Description: declared.Description, Mode: declared.Mode, Enabled: declared.Enabled == nil || *declared.Enabled,
				Gaggle: gaggle.Name, Goober: goober.Name, Harness: string(goober.Spec.Harness), Model: goober.Spec.Model, Instructions: string(instructions), ConfigRevision: revision,
				Tools: append([]string{}, declared.Tools...), Workflows: append([]string{}, declared.Workflows...), AuthorityRefs: append([]string{}, declared.AuthorityRefs...),
				ExecutionModes: append([]string{}, declared.ExecutionModes...), DefaultExecutionMode: declared.DefaultExecutionMode,
				LeaseSeconds: int(declared.Session.LeaseSeconds), MaxTurnSeconds: turnSeconds, MaxChildren: int(declared.Session.MaxChildren), MaxActive: int(declared.Session.MaxActive), MaxActivePerOwner: int(declared.Session.MaxActivePerOwner),
				ChildCommandCapabilities: []string{},
			}
			if declared.ChildWorkflows != nil && declared.ChildWorkflows.Enabled {
				surface.ChildCommandCapabilities = append(surface.ChildCommandCapabilities, declared.ChildWorkflows.CommandCapabilities...)
			}
			if len(surface.ChildCommandCapabilities) == 0 && slices.Contains(surface.Tools, "propose_child_workflow") {
				return nil, fmt.Errorf("surface %s allows child workflow proposal without enabled command capabilities", declared.Name)
			}
			for _, name := range surface.AuthorityRefs {
				authority := bindings[name]
				expected := map[string]string{"ado": "controlled-local-ado", "app-insights": "controlled-local-app-insights"}[authority.Provider]
				if expected == "" || authority.BrokerRef != expected {
					return nil, fmt.Errorf("surface %s authority %s: broker binding %q is unavailable; this worker only has explicitly controlled local providers", declared.Name, name, authority.BrokerRef)
				}
			}
			catalog.Surfaces = append(catalog.Surfaces, surface)
			catalog.Authorities[surface.ID] = bindings
		}
	}
	return catalog, nil
}
