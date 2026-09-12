package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/instance"
)

func TestTopologyRefusesImagesThatActualDispatcherCannotAdmit(t *testing.T) {
	for _, tc := range []struct{ name, image string }{
		{"digest-only", "registry.example.test:5000/goobers@sha256:" + strings.Repeat("a", 64)},
		{"latest", "registry.example.test/goobers:latest@sha256:" + strings.Repeat("a", 64)},
		{"foreign commit", "registry.example.test/goobers:" + strings.Repeat("f", 40) + "@sha256:" + strings.Repeat("a", 64)},
		{"foreign release", "registry.example.test/goobers:v0.3.0@sha256:" + strings.Repeat("a", 64)},
		{"abbreviated commit tag", "registry.example.test/goobers:" + topologyTestCommit[:12] + "@sha256:" + strings.Repeat("a", 64)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := fixture(t)
			setTopologyStageImages(t, o, []string{tc.image})
			runtimeErr := dispatcher.VerifySkew(o.DispatcherCommit, o.DispatcherVersion, tc.image)
			if runtimeErr == nil {
				t.Fatal("fixture must reproduce real dispatch refusal")
			}
			err := prepare(o)
			var skew *dispatcher.SkewError
			if !errors.As(err, &skew) || !strings.Contains(err.Error(), runtimeErr.Error()) || !strings.Contains(err.Error(), "runner linux-pod-0") || !strings.Contains(err.Error(), "tag@digest") {
				t.Fatalf("preparer lost actionable runtime refusal: %v", err)
			}
			if _, err := os.Stat(o.Out); !os.IsNotExist(err) {
				t.Fatal("refused stage image left finalized topology")
			}
		})
	}
}

func TestTopologyAdmitsMixedImageChannelsAgainstExplicitDispatcherStamps(t *testing.T) {
	for _, controlTag := range []string{"", ":" + topologyTestCommit, ":v0.4.0-rc.1"} {
		t.Run(controlTag, func(t *testing.T) {
			o := fixture(t)
			o.Image = "registry.example.test/control-plane" + controlTag + "@sha256:" + strings.Repeat("b", 64)
			stages := []string{
				"registry.example.test:5000/stage:" + topologyTestCommit + "@sha256:" + strings.Repeat("c", 64),
				"registry.example.test/agent-stage:v0.4.0-rc.1@sha256:" + strings.Repeat("d", 64),
			}
			setTopologyStageImages(t, o, stages)
			for _, image := range stages {
				if err := dispatcher.VerifySkew(o.DispatcherCommit, o.DispatcherVersion, image); err != nil {
					t.Fatalf("real dispatcher refuses fixture: %v", err)
				}
			}
			if err := prepare(o); err != nil {
				t.Fatalf("mixed SHA/release stages refused: %v", err)
			}
			if _, err := os.Stat(filepath.Join(o.Out, "kustomization.yaml")); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestTopologyRequiresExplicitValidDispatcherMetadata(t *testing.T) {
	for _, tc := range []struct{ commit, version string }{
		{"", "v0.4.0-rc.1"}, {topologyTestCommit, ""}, {"none", "dev"}, {"badsha!", "dev"}, {"abc123", "dev"}, {strings.Repeat("f", 41), "dev"}, {topologyTestCommit, "v0.4.0-rc.1\n"},
	} {
		o := fixture(t)
		o.DispatcherCommit, o.DispatcherVersion = tc.commit, tc.version
		// A conveniently named image cannot substitute for the actual binary's
		// explicit metadata. Refusal precedes instance loading and output creation.
		o.Image = "registry.example.test/control:" + topologyTestCommit + "@sha256:" + strings.Repeat("a", 64)
		o.Instance = filepath.Join(t.TempDir(), "missing-instance")
		err := prepare(o)
		if err == nil || !strings.Contains(err.Error(), "--dispatcher-commit") || !strings.Contains(err.Error(), "--dispatcher-version") {
			t.Fatalf("missing or invalid metadata was inferred: %v", err)
		}
		if _, err := os.Stat(o.Out); !os.IsNotExist(err) {
			t.Fatal("invalid metadata left finalized topology")
		}
	}
	o := fixture(t)
	o.DispatcherVersion = "dev"
	if err := prepare(o); err != nil {
		t.Fatalf("SHA-tagged stage should accept stamped dev build: %v", err)
	}
}

func setTopologyStageImages(t *testing.T, o options, images []string) {
	t.Helper()
	path := filepath.Join(o.Instance, "instance.yaml")
	cfg, err := instance.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	runner := cfg.Runners[len(cfg.Runners)-1]
	cfg.Runners = nil
	for index, image := range images {
		entry := runner
		entry.Name = fmt.Sprintf("linux-pod-%d", index)
		entry.Host = image
		cfg.Runners = append(cfg.Runners, entry)
	}
	if err := instance.WriteConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
}
